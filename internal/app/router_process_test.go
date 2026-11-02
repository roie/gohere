package app

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestParseProcArgs2Path(t *testing.T) {
	argc := []byte{2, 0, 0, 0}
	tests := []struct {
		name    string
		buf     []byte
		want    string
		wantErr bool
	}{
		{name: "empty", buf: nil, wantErr: true},
		{name: "argc only", buf: argc, wantErr: true},
		{name: "missing terminator", buf: append(append([]byte{}, argc...), "/bin/gohere"...), wantErr: true},
		{name: "empty path", buf: append(append([]byte{}, argc...), 0, 0, 'x', 0), wantErr: true},
		{name: "path with padding and args", buf: append(append([]byte{}, argc...), "/Users/a/.gohere/bin/gohere\x00\x00\x00gohere\x00service\x00run\x00"...), want: "/Users/a/.gohere/bin/gohere"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseProcArgs2Path(tt.buf)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("path = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLinuxExecutableLinkStripsDeletedMarker(t *testing.T) {
	missing := func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	present := func(string) (os.FileInfo, error) { return nil, nil }

	if got := linuxExecutableLink("/home/a/.gohere/bin/gohere (deleted)", missing); got != "/home/a/.gohere/bin/gohere" {
		t.Fatalf("deleted target = %q", got)
	}
	if got := linuxExecutableLink("/home/a/bin/gohere (deleted)", present); got != "/home/a/bin/gohere (deleted)" {
		t.Fatalf("existing file with marker-like name was rewritten to %q", got)
	}
	if got := linuxExecutableLink("/home/a/.gohere/bin/gohere", missing); got != "/home/a/.gohere/bin/gohere" {
		t.Fatalf("plain target = %q", got)
	}
}

func TestWindowsExecutableMatches(t *testing.T) {
	stable := `C:\Users\Alice\.gohere\bin\gohere.exe`
	tests := []struct {
		exe  string
		want bool
	}{
		{exe: stable, want: true},
		{exe: `c:\users\alice\.GOHERE\bin\GOHERE.EXE`, want: true},
		{exe: `C:\Users\Alice\.gohere\bin\gohere.exe.123456.bak`, want: true},
		{exe: `C:\Users\Alice\.gohere\bin\gohere.exe..bak`, want: false},
		{exe: `C:\Users\Alice\.gohere\bin\gohere.exe.123456`, want: false},
		{exe: `C:\Users\Alice\.gohere\bin\other.exe.1.bak`, want: false},
		{exe: `C:\Other\gohere.exe`, want: false},
		{exe: `C:\Other\gohere.exe.1.bak`, want: false},
	}
	for _, tt := range tests {
		if got := windowsExecutableMatches(tt.exe, stable); got != tt.want {
			t.Errorf("windowsExecutableMatches(%q) = %v, want %v", tt.exe, got, tt.want)
		}
	}
}

func TestCompareUnixExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file identity comparison")
	}
	dir := t.TempDir()
	stable := filepath.Join(dir, "gohere")
	other := filepath.Join(dir, "other")
	link := filepath.Join(dir, "link")
	hardLink := filepath.Join(dir, "hard")
	writeFile(t, stable, "a")
	writeFile(t, other, "b")
	if err := os.Symlink(stable, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(stable, hardLink); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		exe  string
		want processState
	}{
		{name: "same path", exe: stable, want: processMatched},
		{name: "symlink", exe: link, want: processMatched},
		{name: "hard link", exe: hardLink, want: processMatched},
		{name: "different file", exe: other, want: processMismatched},
		{name: "deleted stable path", exe: filepath.Join(dir, "removed"), want: processUnknown},
		{name: "missing directory", exe: filepath.Join(dir, "missing", "gohere"), want: processUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := compareUnixExecutable(tt.exe, stable)
			if got != tt.want {
				t.Fatalf("state = %v (err %v), want %v", got, err, tt.want)
			}
			if (err != nil) != (tt.want == processUnknown) {
				t.Fatalf("err = %v for state %v", err, got)
			}
		})
	}

	removedStable := filepath.Join(dir, "removed")
	if got, err := compareUnixExecutable(removedStable, removedStable); got != processMatched || err != nil {
		t.Fatalf("deleted binary at stable path = %v, %v; want matched", got, err)
	}
}

func TestRouterProcessInspectsLiveProcesses(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	process := openRouterProcess(os.Getpid())
	defer process.Close()
	if state, err := process.Inspect(self); state != processMatched {
		t.Fatalf("self = %v, %v; want matched", state, err)
	}

	other := filepath.Join(t.TempDir(), filepath.Base(self))
	copyExecutable(t, self, other)
	if state, err := process.Inspect(other); state != processMismatched {
		t.Fatalf("self against a different binary = %v, %v; want mismatched", state, err)
	}
}

func TestRouterProcessTerminateAndAbsent(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := startProcessHelper(t, self)
	process := openRouterProcess(cmd.Process.Pid)
	defer process.Close()
	if state, err := process.Inspect(self); state != processMatched {
		t.Fatalf("helper = %v, %v; want matched", state, err)
	}
	if err := process.Terminate(); err != nil {
		t.Fatal(err)
	}
	waitForExit(t, cmd)
	if state, err := process.Inspect(self); state != processAbsent {
		t.Fatalf("exited helper = %v, %v; want absent", state, err)
	}
}

func TestRouterProcessMatchesReplacedBinary(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	stable := filepath.Join(dir, stableBinaryName(runtime.GOOS))
	copyExecutable(t, self, stable)
	cmd := startProcessHelper(t, stable)
	process := openRouterProcess(cmd.Process.Pid)
	defer process.Close()
	if state, err := process.Inspect(stable); state != processMatched {
		t.Fatalf("installed helper = %v, %v; want matched", state, err)
	}

	replacement := filepath.Join(dir, "replacement")
	copyExecutable(t, self, replacement)
	if runtime.GOOS == "windows" {
		if err := os.Rename(stable, stable+".42.bak"); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Rename(replacement, stable); err != nil {
		t.Fatal(err)
	}
	if state, err := process.Inspect(stable); state != processMatched {
		t.Fatalf("helper after binary replacement = %v, %v; want matched", state, err)
	}
	if err := process.Terminate(); err != nil {
		t.Fatal(err)
	}
	waitForExit(t, cmd)
}

func startProcessHelper(t *testing.T, executable string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(executable, "-test.run=TestAppCommandHelper", "--", "print-port-sleep")
	cmd.Env = append(os.Environ(), "GOHERE_APP_HELPER_PROCESS=1")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		if cmd.ProcessState == nil {
			_ = cmd.Wait()
		}
	})
	return cmd
}

func waitForExit(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatal("helper process did not exit")
	}
}

func copyExecutable(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}
