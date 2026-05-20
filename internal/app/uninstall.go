package app

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/roie/gohere/internal/admin"
	"github.com/roie/gohere/internal/router"
	"github.com/roie/gohere/internal/setup"
)

type UninstallConfig struct {
	StateDir       string
	ConfigDir      string
	CommandRunner  setup.CommandRunner
	ProcessSignal  func(int) error
	ProcessMatches func(int, string) bool
}

type ServiceStopConfig struct {
	StateDir       string
	ConfigDir      string
	CommandRunner  setup.CommandRunner
	ProcessSignal  func(int) error
	ProcessMatches func(int, string) bool
}

func Uninstall(ctx context.Context, stdout io.Writer) error {
	return UninstallWithConfig(ctx, stdout, UninstallConfig{})
}

func ServiceStop(ctx context.Context, stdout io.Writer) error {
	return ServiceStopWithConfig(ctx, stdout, ServiceStopConfig{})
}

func ServiceStopWithConfig(ctx context.Context, stdout io.Writer, cfg ServiceStopConfig) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg.StateDir == "" {
		cfg.StateDir = router.DefaultStateDir()
	}
	if cfg.ConfigDir == "" {
		cfg.ConfigDir = filepath.Join(homeDir(), ".config")
	}
	if cfg.CommandRunner == nil {
		cfg.CommandRunner = uninstallRealRunner{}
	}
	if cfg.ProcessSignal == nil {
		cfg.ProcessSignal = signalProcess
	}
	if cfg.ProcessMatches == nil {
		cfg.ProcessMatches = processMatchesInstalledBinary
	}

	stopped := false
	if shutdownInstalledService(ctx, cfg.StateDir) == nil {
		stopped = true
	}
	servicePath := filepath.Join(cfg.ConfigDir, "systemd", "user", "gohere-router.service")
	if exists(servicePath) {
		_ = cfg.CommandRunner.Run(ctx, "systemctl", "--user", "stop", "gohere-router")
		stopped = true
	}

	pidPath := filepath.Join(cfg.StateDir, "router.pid")
	if pid, ok := readRouterPID(pidPath); ok {
		stableBinary := filepath.Join(cfg.StateDir, "bin", stableBinaryName(runtime.GOOS))
		if cfg.ProcessMatches(pid, stableBinary) {
			_ = cfg.ProcessSignal(pid)
			stopped = true
		}
	}
	if err := os.Remove(pidPath); err != nil && !os.IsNotExist(err) {
		return err
	}

	if stopped {
		fmt.Fprintln(stdout, "gohere service stopped.")
		return nil
	}
	fmt.Fprintln(stdout, "No gohere service is running.")
	return nil
}

func UninstallWithConfig(ctx context.Context, stdout io.Writer, cfg UninstallConfig) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg.StateDir == "" {
		cfg.StateDir = router.DefaultStateDir()
	}
	if cfg.ConfigDir == "" {
		cfg.ConfigDir = filepath.Join(homeDir(), ".config")
	}
	if cfg.CommandRunner == nil {
		cfg.CommandRunner = uninstallRealRunner{}
	}
	if cfg.ProcessSignal == nil {
		cfg.ProcessSignal = signalProcess
	}
	if cfg.ProcessMatches == nil {
		cfg.ProcessMatches = processMatchesInstalledBinary
	}

	_ = shutdownInstalledService(ctx, cfg.StateDir)
	servicePath := filepath.Join(cfg.ConfigDir, "systemd", "user", "gohere-router.service")
	if exists(servicePath) {
		_ = cfg.CommandRunner.Run(ctx, "systemctl", "--user", "stop", "gohere-router")
		_ = cfg.CommandRunner.Run(ctx, "systemctl", "--user", "disable", "gohere-router")
		if err := os.Remove(servicePath); err != nil && !os.IsNotExist(err) {
			return err
		}
	}

	pidPath := filepath.Join(cfg.StateDir, "router.pid")
	if pid, ok := readRouterPID(pidPath); ok {
		stableBinary := filepath.Join(cfg.StateDir, "bin", stableBinaryName(runtime.GOOS))
		if cfg.ProcessMatches(pid, stableBinary) {
			_ = cfg.ProcessSignal(pid)
		}
	}
	for _, binary := range []string{"gohere", "gohere.exe"} {
		if err := removeInstalledFile(filepath.Join(cfg.StateDir, "bin", binary)); err != nil {
			return err
		}
	}
	if err := os.Remove(pidPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Fprintln(stdout, "gohere service removed.")

	fmt.Fprint(stdout, "Remove gohere local state too? This deletes routes, token, and logs. [y/N] ")
	answer, _ := bufio.NewReader(promptInput).ReadString('\n')
	if shouldRemoveStateFromAnswer(answer) {
		if err := os.RemoveAll(cfg.StateDir); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "gohere local state removed.")
		return nil
	}
	fmt.Fprintln(stdout, "gohere local state kept.")
	return nil
}

func readRouterPID(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

func shouldRemoveStateFromAnswer(answer string) bool {
	answer = strings.TrimSpace(strings.ToLower(answer))
	return answer == "y" || answer == "yes"
}

func shutdownInstalledService(ctx context.Context, stateDir string) error {
	token, err := router.ReadToken(stateDir)
	if err != nil {
		return err
	}
	return adminShutdown(ctx, token)
}

var adminShutdown = func(ctx context.Context, token string) error {
	return admin.NewClient("http://127.0.0.1:39399", token).Shutdown(ctx)
}

func signalProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return terminateProcess(process, runtime.GOOS)
}

type terminableProcess interface {
	Kill() error
	Signal(os.Signal) error
}

func terminateProcess(process terminableProcess, goos string) error {
	if goos == "windows" {
		return process.Kill()
	}
	return process.Signal(syscall.SIGTERM)
}

func processMatchesInstalledBinary(pid int, stableBinary string) bool {
	if pid <= 0 || stableBinary == "" {
		return false
	}
	if runtime.GOOS != "linux" {
		return false
	}
	exe, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err != nil {
		return false
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return false
	}
	stableBinary, err = filepath.EvalSymlinks(stableBinary)
	if err != nil {
		return false
	}
	return exe == stableBinary
}

func removeInstalledFile(path string) error {
	return removePathWithRetry(path, os.Remove, 30, 100*time.Millisecond)
}

func removePathWithRetry(path string, remove func(string) error, attempts int, delay time.Duration) error {
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		err := remove(path)
		if err == nil || os.IsNotExist(err) {
			return nil
		}
		lastErr = err
		if delay > 0 && i < attempts-1 {
			time.Sleep(delay)
		}
	}
	return lastErr
}

type uninstallRealRunner struct{}

func (uninstallRealRunner) Run(ctx context.Context, command string, args ...string) error {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func homeDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return "."
}
