package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUninstallRemovesRouterInstallButKeepsStateByDefault(t *testing.T) {
	stateDir := t.TempDir()
	configDir := t.TempDir()
	writeFile(t, filepath.Join(stateDir, "bin", "gohere"), "binary")
	writeFile(t, filepath.Join(stateDir, "router.pid"), "12345\n")
	writeFile(t, filepath.Join(stateDir, "token"), "token\n")
	writeFile(t, filepath.Join(stateDir, "routes.json"), "[]")
	writeFile(t, filepath.Join(configDir, "systemd", "user", "gohere-router.service"), "service")
	runner := &uninstallRecordingRunner{}
	process := &uninstallRecordingProcess{}
	oldPromptInput := promptInput
	defer func() {
		promptInput = oldPromptInput
	}()
	promptInput = strings.NewReader("\n")

	var out strings.Builder
	if err := UninstallWithConfig(context.Background(), &out, UninstallConfig{
		StateDir:      stateDir,
		ConfigDir:     configDir,
		CommandRunner: runner,
		ProcessSignal: process.Signal,
	}); err != nil {
		t.Fatal(err)
	}

	if exists(filepath.Join(stateDir, "bin", "gohere")) {
		t.Fatal("stable binary still exists")
	}
	if exists(filepath.Join(stateDir, "router.pid")) {
		t.Fatal("router pid still exists")
	}
	if !exists(filepath.Join(stateDir, "token")) {
		t.Fatal("token should be kept by default")
	}
	if !exists(filepath.Join(stateDir, "routes.json")) {
		t.Fatal("routes should be kept by default")
	}
	if exists(filepath.Join(configDir, "systemd", "user", "gohere-router.service")) {
		t.Fatal("systemd service file still exists")
	}
	if !runner.saw("systemctl", "--user", "stop", "gohere-router") {
		t.Fatalf("missing systemctl stop: %#v", runner.commands)
	}
	if !runner.saw("systemctl", "--user", "disable", "gohere-router") {
		t.Fatalf("missing systemctl disable: %#v", runner.commands)
	}
	if !process.saw(12345) {
		t.Fatalf("missing process signal: %#v", process.pids)
	}
	if !strings.Contains(out.String(), "gohere router removed") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestUninstallDeletesAllStateAfterConfirmation(t *testing.T) {
	stateDir := t.TempDir()
	configDir := t.TempDir()
	writeFile(t, filepath.Join(stateDir, "bin", "gohere"), "binary")
	writeFile(t, filepath.Join(stateDir, "token"), "token\n")
	oldPromptInput := promptInput
	defer func() {
		promptInput = oldPromptInput
	}()
	promptInput = strings.NewReader("y\n")

	var out strings.Builder
	if err := UninstallWithConfig(context.Background(), &out, UninstallConfig{
		StateDir:  stateDir,
		ConfigDir: configDir,
	}); err != nil {
		t.Fatal(err)
	}

	if exists(stateDir) {
		t.Fatal("state dir should be removed after confirmation")
	}
}

type uninstallRecordingRunner struct {
	commands [][]string
}

func (r *uninstallRecordingRunner) Run(ctx context.Context, command string, args ...string) error {
	r.commands = append(r.commands, append([]string{command}, args...))
	return nil
}

func (r *uninstallRecordingRunner) saw(items ...string) bool {
	for _, cmd := range r.commands {
		if sameStrings(cmd, items) {
			return true
		}
	}
	return false
}

type uninstallRecordingProcess struct {
	pids []int
}

func (p *uninstallRecordingProcess) Signal(pid int) error {
	p.pids = append(p.pids, pid)
	return nil
}

func (p *uninstallRecordingProcess) saw(pid int) bool {
	for _, got := range p.pids {
		if got == pid {
			return true
		}
	}
	return false
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}
