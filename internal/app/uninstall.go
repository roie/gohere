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
	"strings"
	"time"

	"github.com/roie/gohere/internal/admin"
	localcert "github.com/roie/gohere/internal/cert"
	"github.com/roie/gohere/internal/certtrust"
	appconfig "github.com/roie/gohere/internal/config"
	"github.com/roie/gohere/internal/router"
	"github.com/roie/gohere/internal/setup"
	"github.com/roie/gohere/internal/userpath"
)

type UninstallConfig struct {
	StateDir       string
	ConfigDir      string
	CommandRunner  setup.CommandRunner
	OpenProcess    func(int) routerProcess
	Sleep          func(context.Context, time.Duration) error
	UntrustCA      func(context.Context, string) error
	RemoveUserPath func(context.Context, string) error
	RemoveState    *bool
}

type ServiceStopConfig struct {
	StateDir      string
	ConfigDir     string
	CommandRunner setup.CommandRunner
	OpenProcess   func(int) routerProcess
	Sleep         func(context.Context, time.Duration) error
}

func Uninstall(ctx context.Context, stdout io.Writer) error {
	if detectWSLFunc() {
		return uninstallWSLIntegration(ctx, stdout)
	}
	return UninstallWithConfig(ctx, stdout, UninstallConfig{})
}

func ServiceStop(ctx context.Context, stdout io.Writer) error {
	if detectWSLFunc() {
		resolved, err := resolveWindowsCompanion(ctx, "control.stop-router")
		if err != nil {
			return err
		}
		output, err := resolved.Control.StopRouter(ctx)
		if err != nil {
			return windowsCompanionUnavailableError(err)
		}
		fmt.Fprint(stdout, output)
		return nil
	}
	return ServiceStopWithConfig(ctx, stdout, ServiceStopConfig{})
}

func ServiceStopWithConfig(ctx context.Context, stdout io.Writer, cfg ServiceStopConfig) error {
	if ctx == nil {
		ctx = context.Background()
	}
	stopped, err := stopInstalledRouter(ctx, newRouterStopConfig(cfg.StateDir, cfg.ConfigDir, cfg.CommandRunner, cfg.OpenProcess, cfg.Sleep))
	if err != nil {
		return fmt.Errorf("gohere error: could not stop gohere service: %w", err)
	}
	if stopped {
		fmt.Fprintln(stdout, "gohere service stopped.")
		return nil
	}
	fmt.Fprintln(stdout, "No gohere service is running.")
	return nil
}

func newRouterStopConfig(stateDir, configDir string, runner setup.CommandRunner, openProcess func(int) routerProcess, sleep func(context.Context, time.Duration) error) routerStopConfig {
	cfg := routerStopConfig{stateDir: stateDir, configDir: configDir, commandRunner: runner, openProcess: openProcess, sleep: sleep}
	if cfg.stateDir == "" {
		cfg.stateDir = router.DefaultStateDir()
	}
	if cfg.configDir == "" {
		cfg.configDir = filepath.Join(userpath.HomeDir(), ".config")
	}
	if cfg.commandRunner == nil {
		cfg.commandRunner = uninstallRealRunner{}
	}
	if cfg.openProcess == nil {
		cfg.openProcess = openRouterProcess
	}
	if cfg.sleep == nil {
		cfg.sleep = sleepContext
	}
	return cfg
}

func UninstallWithConfig(ctx context.Context, stdout io.Writer, cfg UninstallConfig) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg.StateDir == "" {
		cfg.StateDir = router.DefaultStateDir()
	}
	if cfg.ConfigDir == "" {
		cfg.ConfigDir = filepath.Join(userpath.HomeDir(), ".config")
	}
	if cfg.CommandRunner == nil {
		cfg.CommandRunner = uninstallRealRunner{}
	}
	if cfg.UntrustCA == nil {
		cfg.UntrustCA = func(ctx context.Context, fingerprint string) error {
			if runtime.GOOS == "linux" && detectWSLFunc() {
				return untrustCAForWSL(ctx, fingerprint)
			}
			return certtrust.UntrustCA(ctx, runtime.GOOS, cfg.CommandRunner, fingerprint)
		}
	}
	if cfg.RemoveUserPath == nil {
		cfg.RemoveUserPath = func(ctx context.Context, binDir string) error {
			if runtime.GOOS != "windows" {
				return nil
			}
			return setup.RemoveWindowsUserPath(ctx, binDir)
		}
	}

	if _, err := stopInstalledRouter(ctx, newRouterStopConfig(cfg.StateDir, cfg.ConfigDir, cfg.CommandRunner, cfg.OpenProcess, cfg.Sleep)); err != nil {
		return fmt.Errorf("gohere error: uninstall stopped before removing anything because the gohere service could not be confirmed stopped: %w", err)
	}

	servicePath := filepath.Join(cfg.ConfigDir, "systemd", "user", "gohere-router.service")
	if exists(servicePath) {
		if err := ctx.Err(); err != nil {
			return err
		}
		_ = cfg.CommandRunner.Run(ctx, "systemctl", "--user", "disable", "gohere-router")
		if err := os.Remove(servicePath); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	binDir := filepath.Join(cfg.StateDir, "bin")
	if err := cfg.RemoveUserPath(ctx, binDir); err != nil {
		return err
	}
	for _, binary := range []string{"gohere", "gohere.exe"} {
		if err := removeInstalledFile(filepath.Join(binDir, binary)); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := untrustHTTPSCAIfEnabled(ctx, cfg.StateDir, cfg.UntrustCA); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "gohere service removed.")

	removeState := false
	if cfg.RemoveState != nil {
		removeState = *cfg.RemoveState
	} else {
		fmt.Fprint(stdout, "\nRemove gohere local state too? This deletes routes, token, and logs. [y/N] ")
		answer, _ := bufio.NewReader(promptInput).ReadString('\n')
		removeState = shouldRemoveStateFromAnswer(answer)
	}
	if removeState {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := os.RemoveAll(cfg.StateDir); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "gohere local state removed.")
		return nil
	}
	fmt.Fprintln(stdout, "gohere local state kept.")
	return nil
}

func untrustHTTPSCAIfEnabled(ctx context.Context, stateDir string, untrustCA func(context.Context, string) error) error {
	cfg, err := appconfig.Load(stateDir)
	if err != nil {
		return err
	}
	if !cfg.HTTPS {
		return nil
	}
	fingerprint, err := caUntrustFingerprint(stateDir)
	if err != nil {
		return err
	}
	return untrustCA(ctx, fingerprint)
}

func caUntrustFingerprint(stateDir string) (string, error) {
	store := localcert.Store{StateDir: stateDir}
	if runtime.GOOS == "linux" && !detectWSLFunc() {
		return store.Fingerprint()
	}
	return store.TrustFingerprint()
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
