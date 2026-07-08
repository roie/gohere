package setup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	appconfig "github.com/roie/gohere/internal/config"
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
	if !runner.saw(stable, "service", "run") {
		t.Fatalf("detached service command missing: %#v", runner.commands)
	}
}

func TestLinuxSetupRestoresStableBinaryModeWhenUpdating(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not preserve Unix executable mode bits")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "source-gohere")
	if err := os.WriteFile(source, []byte("new-binary"), 0755); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(dir, "state")
	stable := filepath.Join(stateDir, "bin", "gohere")
	if err := os.MkdirAll(filepath.Dir(stable), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stable, []byte("old-binary"), 0600); err != nil {
		t.Fatal(err)
	}

	err := Linux(context.Background(), Config{
		StateDir:         stateDir,
		CurrentBinary:    source,
		CommandRunner:    &detachingRunner{pid: 4242},
		SystemdAvailable: false,
	})
	if err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(stable)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0755 {
		t.Fatalf("stable binary mode = %v, want 0755", got)
	}
}

func TestCopyFileKeepsDestinationWhenCopyFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("opening a directory for copy behaves differently on Windows")
	}
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "source-dir")
	if err := os.Mkdir(srcDir, 0755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "stable")
	if err := os.WriteFile(dst, []byte("old-binary"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := copyFile(srcDir, dst, 0755); err == nil {
		t.Fatal("expected copy error")
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "old-binary" {
		t.Fatalf("destination = %q, want old contents", string(data))
	}
}

func TestReplaceInstalledFileForWindowsRestoresExistingTargetWhenReplaceFails(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "stable")
	tmp := filepath.Join(dir, "stable.tmp")
	if err := os.WriteFile(dst, []byte("old-binary"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("new-binary"), 0755); err != nil {
		t.Fatal(err)
	}

	errReplaceFailed := errors.New("replace failed")
	err := replaceInstalledFileForGOOS("windows", tmp, dst, func(oldPath, newPath string) error {
		if oldPath == tmp && newPath == dst {
			return errReplaceFailed
		}
		return os.Rename(oldPath, newPath)
	})
	if !errors.Is(err, errReplaceFailed) {
		t.Fatalf("err = %v, want replace failure", err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "old-binary" {
		t.Fatalf("destination = %q, want old contents restored", string(data))
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

func TestLinuxSetupStopsDetachedServiceWhenRouterPIDWriteFails(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source-gohere")
	if err := os.WriteFile(source, []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(filepath.Join(stateDir, "router.pid"), 0755); err != nil {
		t.Fatal(err)
	}
	oldStop := stopDetachedProcess
	var stopped []int
	stopDetachedProcess = func(pid int) {
		stopped = append(stopped, pid)
	}
	defer func() {
		stopDetachedProcess = oldStop
	}()

	err := Linux(context.Background(), Config{
		StateDir:         stateDir,
		CurrentBinary:    source,
		CommandRunner:    &detachingRunner{pid: 4242},
		SystemdAvailable: false,
	})
	if err == nil {
		t.Fatal("expected router.pid write error")
	}
	if len(stopped) != 1 || stopped[0] != 4242 {
		t.Fatalf("stopped detached pids = %#v, want [4242]", stopped)
	}
}

func TestLinuxSetupDetachedFallbackIsQuiet(t *testing.T) {
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
	if stderr.String() != "" {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestLinuxSetupReportsProgressWhenEnabled(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source-gohere")
	if err := os.WriteFile(source, []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}
	var progress bytes.Buffer

	err := Linux(context.Background(), Config{
		StateDir:         filepath.Join(dir, "state"),
		CurrentBinary:    source,
		CommandRunner:    &detachingRunner{pid: 4242},
		Progress:         &progress,
		SystemdAvailable: false,
	})
	if err != nil {
		t.Fatal(err)
	}

	want := "Installing local service...\n" +
		"Allowing local HTTP/HTTPS ports...\n" +
		"Starting gohere service...\n"
	if progress.String() != want {
		t.Fatalf("progress = %q, want %q", progress.String(), want)
	}
}

func TestLinuxSetupWritesSystemdServiceWhenAvailable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("systemd service files are Linux-specific")
	}
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
	if want := "ExecStart=\"" + filepath.Join(dir, "state", "bin", "gohere") + "\" service run"; !contains(string(data), want) {
		t.Fatalf("service file missing %q:\n%s", want, string(data))
	}
	if !runner.saw("systemctl", "--user", "enable", "--now", "gohere-router") {
		t.Fatalf("commands = %#v", runner.commands)
	}
}

func TestLinuxSetupQuotesSystemdExecStartPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("systemd service files are Linux-specific")
	}
	dir := filepath.Join(t.TempDir(), "path with spaces")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "source-gohere")
	if err := os.WriteFile(source, []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}

	err := Linux(context.Background(), Config{
		StateDir:         filepath.Join(dir, "state"),
		ConfigDir:        filepath.Join(dir, "config"),
		CurrentBinary:    source,
		CommandRunner:    &recordingRunner{},
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
	stable := filepath.Join(dir, "state", "bin", "gohere")
	if want := "ExecStart=\"" + stable + "\" service run"; !contains(string(data), want) {
		t.Fatalf("service file missing quoted ExecStart %q:\n%s", want, string(data))
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
	if !runner.saw(stable, "service", "run") {
		t.Fatalf("detached fallback missing: %#v", runner.commands)
	}
	if !contains(stderr.String(), "systemd start failed; falling back to detached service") {
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

func TestLinuxSetupHTTPSGeneratesCATrustsAndMarksConfig(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source-gohere")
	if err := os.WriteFile(source, []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(dir, "state")
	runner := &detachingRunner{pid: 4242}
	var trustedCAPath string

	err := Linux(context.Background(), Config{
		StateDir:         stateDir,
		CurrentBinary:    source,
		CommandRunner:    runner,
		SystemdAvailable: false,
		HTTPS:            true,
		TrustCA: func(ctx context.Context, caPath string) error {
			trustedCAPath = caPath
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if trustedCAPath != filepath.Join(stateDir, "ca", "ca.pem") {
		t.Fatalf("trusted CA path = %q, want state CA", trustedCAPath)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "ca", "ca.key")); err != nil {
		t.Fatal(err)
	}
	cfg, err := appconfig.Load(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.HTTPS {
		t.Fatalf("config = %#v, want HTTPS enabled", cfg)
	}
	if !runner.saw(filepath.Join(stateDir, "bin", "gohere"), "service", "run") {
		t.Fatalf("detached service command missing: %#v", runner.commands)
	}
}

func TestLinuxSetupHTTPSReportsCertificateProgress(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source-gohere")
	if err := os.WriteFile(source, []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}
	var progress bytes.Buffer

	err := Linux(context.Background(), Config{
		StateDir:         filepath.Join(dir, "state"),
		CurrentBinary:    source,
		CommandRunner:    &detachingRunner{pid: 4242},
		SystemdAvailable: false,
		HTTPS:            true,
		Progress:         &progress,
		TrustCA: func(ctx context.Context, caPath string) error {
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"Setting up HTTPS certificates...\n",
		"Trusting certificate authority in Linux...\n",
		"Installing local service...\n",
		"Starting gohere service...\n",
	} {
		if !contains(progress.String(), want) {
			t.Fatalf("progress missing %q:\n%s", want, progress.String())
		}
	}
}

func TestLinuxSetupHTTPSDoesNotMarkConfigWhenTrustFails(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source-gohere")
	if err := os.WriteFile(source, []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(dir, "state")
	trustErr := errors.New("trust failed")

	err := Linux(context.Background(), Config{
		StateDir:         stateDir,
		CurrentBinary:    source,
		CommandRunner:    &detachingRunner{pid: 4242},
		SystemdAvailable: false,
		HTTPS:            true,
		TrustCA: func(ctx context.Context, caPath string) error {
			return trustErr
		},
	})
	if !errors.Is(err, trustErr) {
		t.Fatalf("err = %v, want trust failure", err)
	}
	cfg, loadErr := appconfig.Load(stateDir)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if cfg.HTTPS {
		t.Fatalf("config = %#v, want HTTPS disabled after trust failure", cfg)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "router.pid")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("router.pid stat err = %v, want not exist", err)
	}
}

func TestWindowsSetupCopiesExeEnsuresTokenAndStartsDetachedRouter(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source-gohere.exe")
	if err := os.WriteFile(source, []byte("windows-binary"), 0755); err != nil {
		t.Fatal(err)
	}
	runner := &detachingRunner{pid: 4242}

	err := Windows(context.Background(), Config{
		StateDir:      filepath.Join(dir, "state"),
		CurrentBinary: source,
		CommandRunner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}

	stable := filepath.Join(dir, "state", "bin", "gohere.exe")
	data, err := os.ReadFile(stable)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "windows-binary" {
		t.Fatalf("stable binary contents = %q", string(data))
	}
	if _, err := os.Stat(filepath.Join(dir, "state", "token")); err != nil {
		t.Fatal(err)
	}
	if !runner.saw(stable, "service", "run") {
		t.Fatalf("detached service command missing: %#v", runner.commands)
	}
	if runner.saw("sudo", "setcap", "cap_net_bind_service=+ep", stable) {
		t.Fatalf("windows setup should not run setcap: %#v", runner.commands)
	}
	data, err = os.ReadFile(filepath.Join(dir, "state", "router.pid"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "4242\n" {
		t.Fatalf("router.pid = %q", string(data))
	}
}

func TestDarwinSetupCopiesBinaryEnsuresTokenAndStartsDetachedRouter(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source-gohere")
	if err := os.WriteFile(source, []byte("darwin-binary"), 0755); err != nil {
		t.Fatal(err)
	}
	runner := &detachingRunner{pid: 4242}

	err := Darwin(context.Background(), Config{
		StateDir:      filepath.Join(dir, "state"),
		CurrentBinary: source,
		CommandRunner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}

	stable := filepath.Join(dir, "state", "bin", "gohere")
	data, err := os.ReadFile(stable)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "darwin-binary" {
		t.Fatalf("stable binary contents = %q", string(data))
	}
	if _, err := os.Stat(filepath.Join(dir, "state", "token")); err != nil {
		t.Fatal(err)
	}
	if !runner.saw(stable, "service", "run") {
		t.Fatalf("detached service command missing: %#v", runner.commands)
	}
	if runner.saw("sudo", "setcap", "cap_net_bind_service=+ep", stable) {
		t.Fatalf("darwin setup should not run setcap: %#v", runner.commands)
	}
	data, err = os.ReadFile(filepath.Join(dir, "state", "router.pid"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "4242\n" {
		t.Fatalf("router.pid = %q", string(data))
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
