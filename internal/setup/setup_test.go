package setup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLinuxSetupCopiesBinaryEnsuresTokenAndRunsSetcap(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source-gohere")
	if err := os.WriteFile(source, []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}

	err := Linux(context.Background(), Config{
		StateDir:         filepath.Join(dir, "state"),
		CurrentBinary:    source,
		CommandRunner:    runner,
		SystemdAvailable: false,
	})
	if err != nil {
		t.Fatal(err)
	}

	stable := filepath.Join(dir, "state", "bin", "gohere")
	data, err := os.ReadFile(stable)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "binary" {
		t.Fatalf("stable binary contents = %q", string(data))
	}
	if _, err := os.Stat(filepath.Join(dir, "state", "token")); err != nil {
		t.Fatal(err)
	}
	if !runner.saw("sudo", "setcap", "cap_net_bind_service=+ep", stable) {
		t.Fatalf("commands = %#v", runner.commands)
	}
	if !runner.saw(stable, "router") {
		t.Fatalf("detached router command missing: %#v", runner.commands)
	}
}

func TestLinuxSetupDetachedFallbackWritesRouterPID(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source-gohere")
	if err := os.WriteFile(source, []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}
	runner := &detachingRunner{pid: 4242}

	err := Linux(context.Background(), Config{
		StateDir:         filepath.Join(dir, "state"),
		CurrentBinary:    source,
		CommandRunner:    runner,
		SystemdAvailable: false,
	})
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "state", "router.pid"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "4242\n" {
		t.Fatalf("router.pid = %q", string(data))
	}
}

func TestLinuxSetupDetachedFallbackWarnsAboutReboot(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source-gohere")
	if err := os.WriteFile(source, []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer

	err := Linux(context.Background(), Config{
		StateDir:         filepath.Join(dir, "state"),
		CurrentBinary:    source,
		CommandRunner:    &detachingRunner{pid: 4242},
		Stderr:           &stderr,
		SystemdAvailable: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(stderr.String(), "router may need restart after reboot") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestLinuxSetupWritesSystemdServiceWhenAvailable(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source-gohere")
	os.WriteFile(source, []byte("binary"), 0755)
	runner := &recordingRunner{}

	err := Linux(context.Background(), Config{
		StateDir:         filepath.Join(dir, "state"),
		ConfigDir:        filepath.Join(dir, "config"),
		CurrentBinary:    source,
		CommandRunner:    runner,
		SystemdAvailable: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	servicePath := filepath.Join(dir, "config", "systemd", "user", "gohere-router.service")
	data, err := os.ReadFile(servicePath)
	if err != nil {
		t.Fatal(err)
	}
	if want := "ExecStart=" + filepath.Join(dir, "state", "bin", "gohere") + " router"; !contains(string(data), want) {
		t.Fatalf("service file missing %q:\n%s", want, string(data))
	}
	if !runner.saw("systemctl", "--user", "enable", "--now", "gohere-router") {
		t.Fatalf("commands = %#v", runner.commands)
	}
}

func TestLinuxSetupFallsBackToDetachedWhenSystemdStartFails(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source-gohere")
	if err := os.WriteFile(source, []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}
	runner := &failingSystemdRunner{detachingRunner: detachingRunner{pid: 4242}}
	var stderr bytes.Buffer

	err := Linux(context.Background(), Config{
		StateDir:         filepath.Join(dir, "state"),
		ConfigDir:        filepath.Join(dir, "config"),
		CurrentBinary:    source,
		CommandRunner:    runner,
		Stderr:           &stderr,
		SystemdAvailable: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	stable := filepath.Join(dir, "state", "bin", "gohere")
	if !runner.saw("systemctl", "--user", "enable", "--now", "gohere-router") {
		t.Fatalf("systemd command missing: %#v", runner.commands)
	}
	if !runner.saw(stable, "router") {
		t.Fatalf("detached fallback missing: %#v", runner.commands)
	}
	if !contains(stderr.String(), "router may need restart after reboot") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestLinuxSetupReusesHealthyRouter(t *testing.T) {
	dir := t.TempDir()
	runner := &recordingRunner{}
	healthCalls := 0

	err := Linux(context.Background(), Config{
		StateDir:      filepath.Join(dir, "state"),
		CurrentBinary: filepath.Join(dir, "missing-binary"),
		CommandRunner: runner,
		RouterHealth: func(context.Context) error {
			healthCalls++
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if healthCalls != 1 {
		t.Fatalf("health calls = %d, want 1", healthCalls)
	}
	if len(runner.commands) != 0 {
		t.Fatalf("setup should not run commands when router is healthy: %#v", runner.commands)
	}
}

type recordingRunner struct {
	commands [][]string
}

func (r *recordingRunner) Run(ctx context.Context, command string, args ...string) error {
	r.commands = append(r.commands, append([]string{command}, args...))
	return nil
}

type detachingRunner struct {
	recordingRunner
	pid int
}

func (r *detachingRunner) StartDetached(ctx context.Context, command string, args ...string) (int, error) {
	r.commands = append(r.commands, append([]string{command}, args...))
	return r.pid, nil
}

type failingSystemdRunner struct {
	detachingRunner
}

func (r *failingSystemdRunner) Run(ctx context.Context, command string, args ...string) error {
	r.commands = append(r.commands, append([]string{command}, args...))
	if command == "systemctl" {
		return errors.New("systemd unavailable")
	}
	return nil
}

func (r *recordingRunner) saw(items ...string) bool {
	for _, cmd := range r.commands {
		if len(cmd) != len(items) {
			continue
		}
		matched := true
		for i := range items {
			if cmd[i] != items[i] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && index(s, sub) >= 0)
}

func index(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
