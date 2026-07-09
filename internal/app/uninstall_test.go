package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	localcert "github.com/roie/gohere/internal/cert"
	appconfig "github.com/roie/gohere/internal/config"
)

func TestUninstallRemovesRouterInstallButKeepsStateByDefault(t *testing.T) {
	stateDir := t.TempDir()
	configDir := t.TempDir()
	writeFile(t, filepath.Join(stateDir, "bin", "gohere"), "binary")
	writeFile(t, filepath.Join(stateDir, "bin", "gohere.exe"), "binary")
	writeFile(t, filepath.Join(stateDir, "router.pid"), "12345\n")
	writeFile(t, filepath.Join(stateDir, "token"), "token\n")
	writeFile(t, filepath.Join(stateDir, "routes.json"), "[]")
	writeFile(t, filepath.Join(configDir, "systemd", "user", "gohere-router.service"), "service")
	runner := &uninstallRecordingRunner{}
	process := &uninstallRecordingProcess{}
	oldPromptInput := promptInput
	oldAdminShutdown := adminShutdown
	defer func() {
		promptInput = oldPromptInput
		adminShutdown = oldAdminShutdown
	}()
	adminShutdown = func(context.Context, string) error { return errors.New("service unavailable") }
	promptInput = strings.NewReader("\n")

	var out strings.Builder
	if err := UninstallWithConfig(context.Background(), &out, UninstallConfig{
		StateDir:      stateDir,
		ConfigDir:     configDir,
		CommandRunner: runner,
		ProcessSignal: process.Signal,
		ProcessMatches: func(pid int, binary string) bool {
			return pid == 12345 && strings.HasSuffix(binary, filepath.Join("bin", stableBinaryName(runtime.GOOS)))
		},
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
	if !strings.Contains(out.String(), "gohere service removed") {
		t.Fatalf("output = %q", out.String())
	}
	if !strings.Contains(out.String(), "gohere service removed.\n\nRemove gohere local state too?") {
		t.Fatalf("state prompt should be separated from removal output: %q", out.String())
	}
}

func TestServiceStopStopsRuntimeButKeepsInstallAndState(t *testing.T) {
	stateDir := t.TempDir()
	configDir := t.TempDir()
	writeFile(t, filepath.Join(stateDir, "bin", "gohere"), "binary")
	writeFile(t, filepath.Join(stateDir, "bin", "gohere.exe"), "binary")
	writeFile(t, filepath.Join(stateDir, "router.pid"), "12345\n")
	writeFile(t, filepath.Join(stateDir, "token"), "token\n")
	writeFile(t, filepath.Join(stateDir, "routes.json"), "[]")
	writeFile(t, filepath.Join(configDir, "systemd", "user", "gohere-router.service"), "service")
	runner := &uninstallRecordingRunner{}
	process := &uninstallRecordingProcess{}
	oldAdminShutdown := adminShutdown
	defer func() {
		adminShutdown = oldAdminShutdown
	}()
	adminShutdown = func(context.Context, string) error { return errors.New("service unavailable") }

	var out strings.Builder
	if err := ServiceStopWithConfig(context.Background(), &out, ServiceStopConfig{
		StateDir:      stateDir,
		ConfigDir:     configDir,
		CommandRunner: runner,
		ProcessSignal: process.Signal,
		ProcessMatches: func(pid int, binary string) bool {
			return pid == 12345 && strings.HasSuffix(binary, filepath.Join("bin", stableBinaryName(runtime.GOOS)))
		},
	}); err != nil {
		t.Fatal(err)
	}

	if !exists(filepath.Join(stateDir, "bin", "gohere")) {
		t.Fatal("stable binary should be kept")
	}
	if exists(filepath.Join(stateDir, "router.pid")) {
		t.Fatal("router pid should be removed")
	}
	if !exists(filepath.Join(stateDir, "token")) {
		t.Fatal("token should be kept")
	}
	if !exists(filepath.Join(stateDir, "routes.json")) {
		t.Fatal("routes should be kept")
	}
	if !exists(filepath.Join(configDir, "systemd", "user", "gohere-router.service")) {
		t.Fatal("systemd service file should be kept")
	}
	if !runner.saw("systemctl", "--user", "stop", "gohere-router") {
		t.Fatalf("missing systemctl stop: %#v", runner.commands)
	}
	if runner.saw("systemctl", "--user", "disable", "gohere-router") {
		t.Fatalf("service stop should not disable systemd service: %#v", runner.commands)
	}
	if !process.saw(12345) {
		t.Fatalf("missing process signal: %#v", process.pids)
	}
	if !strings.Contains(out.String(), "gohere service stopped") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestServiceStopReportsSystemdStopFailure(t *testing.T) {
	stateDir := t.TempDir()
	configDir := t.TempDir()
	writeFile(t, filepath.Join(configDir, "systemd", "user", "gohere-router.service"), "service")
	runner := &uninstallRecordingRunner{err: errors.New("systemctl failed")}
	oldAdminShutdown := adminShutdown
	defer func() {
		adminShutdown = oldAdminShutdown
	}()
	adminShutdown = func(context.Context, string) error { return errors.New("service unavailable") }

	var out strings.Builder
	err := ServiceStopWithConfig(context.Background(), &out, ServiceStopConfig{
		StateDir:      stateDir,
		ConfigDir:     configDir,
		CommandRunner: runner,
	})
	if err == nil || !strings.Contains(err.Error(), "systemctl failed") {
		t.Fatalf("error = %v, want systemctl failure", err)
	}
	if strings.Contains(out.String(), "gohere service stopped") {
		t.Fatalf("output claimed success: %q", out.String())
	}
}

func TestServiceStopDoesNotSignalUnverifiedPID(t *testing.T) {
	stateDir := t.TempDir()
	configDir := t.TempDir()
	writeFile(t, filepath.Join(stateDir, "router.pid"), "12345\n")
	process := &uninstallRecordingProcess{}
	oldAdminShutdown := adminShutdown
	defer func() {
		adminShutdown = oldAdminShutdown
	}()
	adminShutdown = func(context.Context, string) error { return errors.New("service unavailable") }

	var out strings.Builder
	if err := ServiceStopWithConfig(context.Background(), &out, ServiceStopConfig{
		StateDir:       stateDir,
		ConfigDir:      configDir,
		ProcessSignal:  process.Signal,
		ProcessMatches: func(int, string) bool { return false },
	}); err != nil {
		t.Fatal(err)
	}

	if len(process.pids) != 0 {
		t.Fatalf("unexpected process signals: %#v", process.pids)
	}
	if strings.TrimSpace(out.String()) != "No gohere service is running." {
		t.Fatalf("output = %q", out.String())
	}
	if exists(filepath.Join(stateDir, "router.pid")) {
		t.Fatal("unverified stale pid file should still be removed")
	}
}

func TestServiceStopUsesAdminShutdownBeforePIDFallback(t *testing.T) {
	stateDir := t.TempDir()
	configDir := t.TempDir()
	writeFile(t, filepath.Join(stateDir, "token"), strings.Repeat("a", 64)+"\n")
	writeFile(t, filepath.Join(stateDir, "router.pid"), "12345\n")
	process := &uninstallRecordingProcess{}
	called := false
	oldAdminShutdown := adminShutdown
	defer func() {
		adminShutdown = oldAdminShutdown
	}()
	adminShutdown = func(ctx context.Context, token string) error {
		called = true
		if token != strings.Repeat("a", 64) {
			t.Fatalf("token = %q", token)
		}
		return nil
	}

	var out strings.Builder
	if err := ServiceStopWithConfig(context.Background(), &out, ServiceStopConfig{
		StateDir:       stateDir,
		ConfigDir:      configDir,
		ProcessSignal:  process.Signal,
		ProcessMatches: func(int, string) bool { return false },
	}); err != nil {
		t.Fatal(err)
	}

	if !called {
		t.Fatal("admin shutdown was not called")
	}
	if len(process.pids) != 0 {
		t.Fatalf("unexpected process signals: %#v", process.pids)
	}
	if !strings.Contains(out.String(), "gohere service stopped") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestServiceStopReportsNoServiceWhenNothingIsRunning(t *testing.T) {
	stateDir := t.TempDir()
	configDir := t.TempDir()
	runner := &uninstallRecordingRunner{}
	process := &uninstallRecordingProcess{}

	var out strings.Builder
	if err := ServiceStopWithConfig(context.Background(), &out, ServiceStopConfig{
		StateDir:      stateDir,
		ConfigDir:     configDir,
		CommandRunner: runner,
		ProcessSignal: process.Signal,
	}); err != nil {
		t.Fatal(err)
	}

	if len(runner.commands) != 0 {
		t.Fatalf("unexpected commands: %#v", runner.commands)
	}
	if len(process.pids) != 0 {
		t.Fatalf("unexpected process signals: %#v", process.pids)
	}
	if strings.TrimSpace(out.String()) != "No gohere service is running." {
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

func TestUninstallUntrustsHTTPSCAWhenEnabled(t *testing.T) {
	stateDir := t.TempDir()
	configDir := t.TempDir()
	if err := appconfig.Save(stateDir, appconfig.Config{HTTPS: true}); err != nil {
		t.Fatal(err)
	}
	store := localcert.Store{StateDir: stateDir}
	if _, err := store.EnsureCA(); err != nil {
		t.Fatal(err)
	}
	wantFingerprint, err := caUntrustFingerprint(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	oldPromptInput := promptInput
	defer func() {
		promptInput = oldPromptInput
	}()
	promptInput = strings.NewReader("\n")
	var fingerprints []string

	var out strings.Builder
	if err := UninstallWithConfig(context.Background(), &out, UninstallConfig{
		StateDir:  stateDir,
		ConfigDir: configDir,
		UntrustCA: func(ctx context.Context, fingerprint string) error {
			fingerprints = append(fingerprints, fingerprint)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	if len(fingerprints) != 1 || fingerprints[0] != wantFingerprint {
		t.Fatalf("untrusted fingerprints = %#v, want %q", fingerprints, wantFingerprint)
	}
}

func TestUninstallDoesNotUntrustWhenHTTPSDisabled(t *testing.T) {
	stateDir := t.TempDir()
	configDir := t.TempDir()
	if err := appconfig.Save(stateDir, appconfig.Config{HTTPS: false}); err != nil {
		t.Fatal(err)
	}
	oldPromptInput := promptInput
	defer func() {
		promptInput = oldPromptInput
	}()
	promptInput = strings.NewReader("\n")

	var out strings.Builder
	if err := UninstallWithConfig(context.Background(), &out, UninstallConfig{
		StateDir:  stateDir,
		ConfigDir: configDir,
		UntrustCA: func(ctx context.Context, fingerprint string) error {
			t.Fatal("untrust should not run when HTTPS is disabled")
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUninstallRemovesWindowsStableBinary(t *testing.T) {
	stateDir := t.TempDir()
	configDir := t.TempDir()
	writeFile(t, filepath.Join(stateDir, "bin", "gohere.exe"), "binary")
	oldPromptInput := promptInput
	defer func() {
		promptInput = oldPromptInput
	}()
	promptInput = strings.NewReader("\n")

	var out strings.Builder
	if err := UninstallWithConfig(context.Background(), &out, UninstallConfig{
		StateDir:  stateDir,
		ConfigDir: configDir,
	}); err != nil {
		t.Fatal(err)
	}

	if exists(filepath.Join(stateDir, "bin", "gohere.exe")) {
		t.Fatal("windows stable binary still exists")
	}
}

func TestTerminateProcessUsesKillOnWindows(t *testing.T) {
	process := &fakeProcess{}

	if err := terminateProcess(process, "windows"); err != nil {
		t.Fatal(err)
	}
	if !process.killed {
		t.Fatal("expected Windows process termination to use Kill")
	}
	if process.signaled != nil {
		t.Fatalf("expected no signal, got %v", process.signaled)
	}
}

func TestProcessMatchesInstalledBinaryUsesWindowsExecutablePath(t *testing.T) {
	oldWindowsProcessExecutable := windowsProcessExecutable
	defer func() {
		windowsProcessExecutable = oldWindowsProcessExecutable
	}()
	windowsProcessExecutable = func(pid int) (string, bool) {
		if pid != 12345 {
			return "", false
		}
		return `C:\Users\Jessa\.gohere\bin\gohere.exe`, true
	}

	if !processMatchesInstalledBinaryForGOOS("windows", 12345, `C:\Users\Jessa\.gohere\bin\gohere.exe`) {
		t.Fatal("expected Windows process executable to match stable binary")
	}
	if processMatchesInstalledBinaryForGOOS("windows", 12345, `C:\Other\gohere.exe`) {
		t.Fatal("expected different Windows executable to be rejected")
	}
}

func TestTerminateProcessUsesSigtermOnLinux(t *testing.T) {
	process := &fakeProcess{}

	if err := terminateProcess(process, "linux"); err != nil {
		t.Fatal(err)
	}
	if process.killed {
		t.Fatal("expected Linux process termination not to use Kill")
	}
	if process.signaled != syscall.SIGTERM {
		t.Fatalf("signal = %v, want SIGTERM", process.signaled)
	}
}

func TestRemovePathWithRetryRetriesTemporaryFailure(t *testing.T) {
	attempts := 0
	errBusy := errors.New("busy")

	err := removePathWithRetry("binary", func(path string) error {
		if path != "binary" {
			t.Fatalf("path = %q", path)
		}
		attempts++
		if attempts < 3 {
			return errBusy
		}
		return nil
	}, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestRemovePathWithRetryIgnoresMissingPath(t *testing.T) {
	err := removePathWithRetry("missing", func(string) error {
		return os.ErrNotExist
	}, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
}

type uninstallRecordingRunner struct {
	commands [][]string
	err      error
}

func (r *uninstallRecordingRunner) Run(ctx context.Context, command string, args ...string) error {
	r.commands = append(r.commands, append([]string{command}, args...))
	return r.err
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

type fakeProcess struct {
	killed   bool
	signaled os.Signal
}

func (p *fakeProcess) Kill() error {
	p.killed = true
	return nil
}

func (p *fakeProcess) Signal(signal os.Signal) error {
	p.signaled = signal
	return nil
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
