package certtrust

import (
	"context"
	"testing"
)

func TestTrustCALinuxCommands(t *testing.T) {
	runner := &recordingRunner{}

	if err := TrustCA(context.Background(), "linux", runner, "/tmp/ca.pem"); err != nil {
		t.Fatal(err)
	}

	runner.want(t,
		[]string{"sudo", "mkdir", "-p", "/usr/local/share/ca-certificates"},
		[]string{"sudo", "cp", "/tmp/ca.pem", "/usr/local/share/ca-certificates/gohere-local-ca.crt"},
		[]string{"sudo", "update-ca-certificates"},
	)
}

func TestTrustCADarwinCommand(t *testing.T) {
	runner := &recordingRunner{}

	if err := TrustCA(context.Background(), "darwin", runner, "/tmp/ca.pem"); err != nil {
		t.Fatal(err)
	}

	runner.want(t, []string{"sudo", "security", "add-trusted-cert", "-d", "-r", "trustRoot", "-k", "/Library/Keychains/System.keychain", "/tmp/ca.pem"})
}

func TestTrustCAWindowsCommand(t *testing.T) {
	runner := &recordingRunner{}

	if err := TrustCA(context.Background(), "windows", runner, `C:\Temp\ca.pem`); err != nil {
		t.Fatal(err)
	}

	runner.want(t, []string{"certutil", "-f", "-user", "-addstore", "Root", `C:\Temp\ca.pem`})
}

func TestUntrustCALinuxCommands(t *testing.T) {
	runner := &recordingRunner{}

	if err := UntrustCA(context.Background(), "linux", runner, "abc123"); err != nil {
		t.Fatal(err)
	}

	runner.want(t,
		[]string{"sudo", "rm", "-f", "/usr/local/share/ca-certificates/gohere-local-ca.crt"},
		[]string{"sudo", "update-ca-certificates"},
	)
}

func TestUntrustCADarwinUsesFingerprint(t *testing.T) {
	runner := &recordingRunner{}

	if err := UntrustCA(context.Background(), "darwin", runner, "abc123"); err != nil {
		t.Fatal(err)
	}

	runner.want(t, []string{"sudo", "security", "delete-certificate", "-Z", "abc123", "/Library/Keychains/System.keychain"})
}

func TestUntrustCAWindowsUsesFingerprint(t *testing.T) {
	runner := &recordingRunner{}

	if err := UntrustCA(context.Background(), "windows", runner, "abc123"); err != nil {
		t.Fatal(err)
	}

	runner.want(t, []string{"certutil", "-f", "-user", "-delstore", "Root", "abc123"})
}

type recordingRunner struct {
	commands [][]string
}

func (r *recordingRunner) Run(ctx context.Context, command string, args ...string) error {
	r.commands = append(r.commands, append([]string{command}, args...))
	return nil
}

func (r *recordingRunner) want(t *testing.T, commands ...[]string) {
	t.Helper()
	if len(r.commands) != len(commands) {
		t.Fatalf("commands = %#v, want %#v", r.commands, commands)
	}
	for i := range commands {
		if len(r.commands[i]) != len(commands[i]) {
			t.Fatalf("command %d = %#v, want %#v", i, r.commands[i], commands[i])
		}
		for j := range commands[i] {
			if r.commands[i][j] != commands[i][j] {
				t.Fatalf("command %d = %#v, want %#v", i, r.commands[i], commands[i])
			}
		}
	}
}
