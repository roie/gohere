package certtrust

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

type linuxHostSpec struct {
	dirs     []string
	files    []string
	commands []string
}

func simulateLinuxHost(t *testing.T, spec linuxHostSpec) {
	t.Helper()
	old := LinuxHost
	t.Cleanup(func() { LinuxHost = old })
	LinuxHost.DirExists = func(path string) bool { return slices.Contains(spec.dirs, path) }
	LinuxHost.FileExists = func(path string) bool { return slices.Contains(spec.files, path) }
	LinuxHost.CommandExists = func(name string) bool { return slices.Contains(spec.commands, name) }
}

var (
	debianHost = linuxHostSpec{dirs: []string{"/usr/local/share/ca-certificates"}, commands: []string{"update-ca-certificates"}}
	fedoraHost = linuxHostSpec{dirs: []string{"/etc/pki/ca-trust/source/anchors"}, commands: []string{"update-ca-trust"}}
)

func TestTrustCALinuxUsesDetectedTrustStore(t *testing.T) {
	tests := []struct {
		name string
		host linuxHostSpec
		want [][]string
	}{
		{"debian", debianHost, [][]string{
			{"sudo", "cp", "/tmp/ca.pem", "/usr/local/share/ca-certificates/gohere-local-ca.crt"},
			{"sudo", "update-ca-certificates"},
		}},
		{"debian without ca-certificates directory", linuxHostSpec{commands: []string{"update-ca-certificates"}}, [][]string{
			{"sudo", "mkdir", "-p", "/usr/local/share/ca-certificates"},
			{"sudo", "cp", "/tmp/ca.pem", "/usr/local/share/ca-certificates/gohere-local-ca.crt"},
			{"sudo", "update-ca-certificates"},
		}},
		{"fedora", fedoraHost, [][]string{
			{"sudo", "cp", "/tmp/ca.pem", "/etc/pki/ca-trust/source/anchors/gohere-local-ca.crt"},
			{"sudo", "update-ca-trust", "extract"},
		}},
		{"arch", linuxHostSpec{dirs: []string{"/etc/ca-certificates/trust-source/anchors"}, commands: []string{"update-ca-trust"}}, [][]string{
			{"sudo", "cp", "/tmp/ca.pem", "/etc/ca-certificates/trust-source/anchors/gohere-local-ca.crt"},
			{"sudo", "update-ca-trust", "extract"},
		}},
		{"opensuse", linuxHostSpec{dirs: []string{"/etc/pki/trust/anchors"}, commands: []string{"update-ca-certificates"}}, [][]string{
			{"sudo", "cp", "/tmp/ca.pem", "/etc/pki/trust/anchors/gohere-local-ca.crt"},
			{"sudo", "update-ca-certificates"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			simulateLinuxHost(t, tt.host)
			runner := &recordingRunner{}
			if err := TrustCA(t.Context(), "linux", runner, "/tmp/ca.pem"); err != nil {
				t.Fatal(err)
			}
			runner.want(t, tt.want...)
		})
	}
}

func TestTrustCALinuxWithoutTrustStoreFailsBeforeChangingAnything(t *testing.T) {
	simulateLinuxHost(t, linuxHostSpec{})
	runner := &recordingRunner{}

	err := TrustCA(t.Context(), "linux", runner, "/tmp/ca.pem")
	if !errors.Is(err, errNoLinuxTrustStore) || !strings.Contains(err.Error(), "update-ca-trust") {
		t.Fatalf("err = %v", err)
	}
	runner.want(t)
}

func TestTrustCALinuxRollsBackWhenRefreshFails(t *testing.T) {
	simulateLinuxHost(t, fedoraHost)
	refreshErr := errors.New("update-ca-trust failed")
	runner := &recordingRunner{fail: map[string]error{"update-ca-trust": refreshErr}, failOnce: true}

	err := TrustCA(t.Context(), "linux", runner, "/tmp/ca.pem")
	if !errors.Is(err, refreshErr) {
		t.Fatalf("err = %v", err)
	}
	runner.want(t,
		[]string{"sudo", "cp", "/tmp/ca.pem", "/etc/pki/ca-trust/source/anchors/gohere-local-ca.crt"},
		[]string{"sudo", "update-ca-trust", "extract"},
		[]string{"sudo", "rm", "-f", "/etc/pki/ca-trust/source/anchors/gohere-local-ca.crt"},
		[]string{"sudo", "update-ca-trust", "extract"},
	)
}

func TestTrustCALinuxRollsBackWhenCopyFails(t *testing.T) {
	simulateLinuxHost(t, debianHost)
	copyErr := errors.New("disk full")
	runner := &recordingRunner{fail: map[string]error{"cp": copyErr}}

	err := TrustCA(t.Context(), "linux", runner, "/tmp/ca.pem")
	if !errors.Is(err, copyErr) {
		t.Fatalf("err = %v", err)
	}
	runner.want(t,
		[]string{"sudo", "cp", "/tmp/ca.pem", "/usr/local/share/ca-certificates/gohere-local-ca.crt"},
		[]string{"sudo", "rm", "-f", "/usr/local/share/ca-certificates/gohere-local-ca.crt"},
		[]string{"sudo", "update-ca-certificates"},
	)
}

func TestUntrustCALinuxRemovesFromDetectedTrustStore(t *testing.T) {
	tests := []struct {
		name string
		host linuxHostSpec
		want [][]string
	}{
		{"debian", linuxHostSpec{dirs: debianHost.dirs, commands: debianHost.commands, files: []string{"/usr/local/share/ca-certificates/gohere-local-ca.crt"}}, [][]string{
			{"sudo", "rm", "-f", "/usr/local/share/ca-certificates/gohere-local-ca.crt"},
			{"sudo", "update-ca-certificates"},
		}},
		{"fedora", linuxHostSpec{dirs: fedoraHost.dirs, commands: fedoraHost.commands, files: []string{"/etc/pki/ca-trust/source/anchors/gohere-local-ca.crt"}}, [][]string{
			{"sudo", "rm", "-f", "/etc/pki/ca-trust/source/anchors/gohere-local-ca.crt"},
			{"sudo", "update-ca-trust", "extract"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			simulateLinuxHost(t, tt.host)
			runner := &recordingRunner{}
			if err := UntrustCA(t.Context(), "linux", runner, "abc123"); err != nil {
				t.Fatal(err)
			}
			runner.want(t, tt.want...)
		})
	}
}

func TestUntrustCALinuxWithoutTrustStoreOrCAChangesNothing(t *testing.T) {
	simulateLinuxHost(t, linuxHostSpec{})
	runner := &recordingRunner{}

	if err := UntrustCA(t.Context(), "linux", runner, "abc123"); err != nil {
		t.Fatal(err)
	}
	runner.want(t)
}

func TestInstalledLinuxCAPathFindsFedoraAnchor(t *testing.T) {
	simulateLinuxHost(t, linuxHostSpec{files: []string{"/etc/pki/ca-trust/source/anchors/gohere-local-ca.crt"}})
	if got := InstalledLinuxCAPath(); got != "/etc/pki/ca-trust/source/anchors/gohere-local-ca.crt" {
		t.Fatalf("path = %q", got)
	}
	simulateLinuxHost(t, linuxHostSpec{})
	if got := InstalledLinuxCAPath(); got != "" {
		t.Fatalf("path = %q, want none", got)
	}
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
	fail     map[string]error
	failOnce bool
}

func (r *recordingRunner) Run(ctx context.Context, command string, args ...string) error {
	r.commands = append(r.commands, append([]string{command}, args...))
	if len(args) > 0 {
		if err, ok := r.fail[args[0]]; ok {
			if r.failOnce {
				delete(r.fail, args[0])
			}
			return err
		}
	}
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
