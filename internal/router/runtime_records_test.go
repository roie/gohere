package router

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestReadPIDRecordStates(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		want     PIDRecordState
		wantPID  int
	}{
		{name: "plain", contents: "123\n", want: PIDRecordPresent, wantPID: 123},
		{name: "surrounding whitespace", contents: "  42 \r\n", want: PIDRecordPresent, wantPID: 42},
		{name: "empty", contents: "", want: PIDRecordUnknown},
		{name: "whitespace only", contents: " \n", want: PIDRecordUnknown},
		{name: "not a number", contents: "abc\n", want: PIDRecordUnknown},
		{name: "extra data", contents: "12 13\n", want: PIDRecordUnknown},
		{name: "zero", contents: "0\n", want: PIDRecordUnknown},
		{name: "negative", contents: "-5\n", want: PIDRecordUnknown},
		{name: "signed", contents: "+5\n", want: PIDRecordUnknown},
		{name: "overflow", contents: strings.Repeat("9", 40) + "\n", want: PIDRecordUnknown},
		{name: "oversized", contents: strings.Repeat(" ", 100) + "7", want: PIDRecordUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stateDir := t.TempDir()
			writeRecord(t, stateDir, RouterPIDFilename, tt.contents)
			record := ReadPIDRecord(stateDir)
			if record.State != tt.want || record.PID != tt.wantPID {
				t.Fatalf("record = %+v, want state %v pid %d", record, tt.want, tt.wantPID)
			}
			if (record.Err != nil) != (tt.want == PIDRecordUnknown) {
				t.Fatalf("record error = %v for state %v", record.Err, record.State)
			}
		})
	}

	if record := ReadPIDRecord(t.TempDir()); record.State != PIDRecordMissing || record.Err != nil {
		t.Fatalf("missing record = %+v", record)
	}
	unreadable := t.TempDir()
	if err := os.Mkdir(filepath.Join(unreadable, RouterPIDFilename), 0700); err != nil {
		t.Fatal(err)
	}
	if record := ReadPIDRecord(unreadable); record.State != PIDRecordUnknown || record.Err == nil {
		t.Fatalf("unreadable record = %+v", record)
	}
}

func TestWritePIDRecordRoundTrips(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	if err := WritePIDRecord(stateDir, 4321); err != nil {
		t.Fatal(err)
	}
	if record := ReadPIDRecord(stateDir); record.State != PIDRecordPresent || record.PID != 4321 {
		t.Fatalf("record = %+v", record)
	}
}

func TestRemovePIDRecordIf(t *testing.T) {
	t.Run("unchanged", func(t *testing.T) {
		stateDir := t.TempDir()
		writeRecord(t, stateDir, RouterPIDFilename, "100\n")
		if err := RemovePIDRecordIf(stateDir, 100); err != nil {
			t.Fatal(err)
		}
		if record := ReadPIDRecord(stateDir); record.State != PIDRecordMissing {
			t.Fatalf("record = %+v, want missing", record)
		}
	})
	t.Run("missing", func(t *testing.T) {
		if err := RemovePIDRecordIf(t.TempDir(), 100); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("changed", func(t *testing.T) {
		stateDir := t.TempDir()
		writeRecord(t, stateDir, RouterPIDFilename, "200\n")
		if err := RemovePIDRecordIf(stateDir, 100); !errors.Is(err, ErrRuntimeRecordChanged) {
			t.Fatalf("err = %v, want ErrRuntimeRecordChanged", err)
		}
		if record := ReadPIDRecord(stateDir); record.PID != 200 {
			t.Fatalf("replacement record = %+v, want preserved", record)
		}
	})
	t.Run("malformed", func(t *testing.T) {
		stateDir := t.TempDir()
		writeRecord(t, stateDir, RouterPIDFilename, "garbage\n")
		if err := RemovePIDRecordIf(stateDir, 100); err == nil {
			t.Fatal("expected malformed record error")
		}
		if _, err := os.Stat(filepath.Join(stateDir, RouterPIDFilename)); err != nil {
			t.Fatalf("malformed record was not preserved: %v", err)
		}
	})
}

func TestReleaseRuntimeRecordsRemovesOnlyOwnRecords(t *testing.T) {
	stateDir := t.TempDir()
	writeRecord(t, stateDir, RouterPIDFilename, "111\n")
	writeRecord(t, stateDir, RouterInstanceFilename, "old\n")
	if err := releaseRuntimeRecords(stateDir, 111, "old"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{RouterPIDFilename, RouterInstanceFilename} {
		if _, err := os.Stat(filepath.Join(stateDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s stat err = %v, want removed", name, err)
		}
	}

	writeRecord(t, stateDir, RouterPIDFilename, "222\n")
	writeRecord(t, stateDir, RouterInstanceFilename, "new\n")
	if err := releaseRuntimeRecords(stateDir, 111, "old"); !errors.Is(err, ErrRuntimeRecordChanged) {
		t.Fatalf("err = %v, want ErrRuntimeRecordChanged", err)
	}
	assertRecord(t, stateDir, RouterPIDFilename, "222\n")
	assertRecord(t, stateDir, RouterInstanceFilename, "new\n")
}

func TestRemovePIDRecordIfWaitsForRuntimeLock(t *testing.T) {
	stateDir := t.TempDir()
	writeRecord(t, stateDir, RouterPIDFilename, "100\n")
	unlock, err := LockRuntimeRecords(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- RemovePIDRecordIf(stateDir, 100) }()
	select {
	case err := <-done:
		unlock()
		t.Fatalf("removal finished while the runtime lock was held: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	assertRecord(t, stateDir, RouterPIDFilename, "100\n")
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReleaseRuntimeRecordsDoesNotRecreateRemovedStateDir(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	if err := releaseRuntimeRecords(stateDir, 1, "id"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state dir stat err = %v, want not exist", err)
	}
}

func TestRunningClosePreservesNewerRouterRecords(t *testing.T) {
	stateDir := t.TempDir()
	running, err := Start(t.Context(), StartConfig{
		HTTPAddr:  "127.0.0.1:0",
		AdminAddr: "127.0.0.1:0",
		StateDir:  stateDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertRecord(t, stateDir, RouterPIDFilename, strconv.Itoa(os.Getpid())+"\n")

	writeRecord(t, stateDir, RouterPIDFilename, "999999\n")
	writeRecord(t, stateDir, RouterInstanceFilename, "newer-router\n")
	if err := running.Close(); err != nil {
		t.Fatal(err)
	}
	assertRecord(t, stateDir, RouterPIDFilename, "999999\n")
	assertRecord(t, stateDir, RouterInstanceFilename, "newer-router\n")
}

func TestRunningCloseRemovesOwnRecords(t *testing.T) {
	stateDir := t.TempDir()
	running, err := Start(t.Context(), StartConfig{
		HTTPAddr:  "127.0.0.1:0",
		AdminAddr: "127.0.0.1:0",
		StateDir:  stateDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := running.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{RouterPIDFilename, RouterInstanceFilename} {
		if _, err := os.Stat(filepath.Join(stateDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s stat err = %v, want removed", name, err)
		}
	}
}

func writeRecord(t *testing.T, stateDir, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(stateDir, name), []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func assertRecord(t *testing.T, stateDir, name, want string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(stateDir, name))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", name, data, want)
	}
}
