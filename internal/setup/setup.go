package setup

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"

	localcert "github.com/roie/gohere/internal/cert"
	"github.com/roie/gohere/internal/certtrust"
	appconfig "github.com/roie/gohere/internal/config"
	"github.com/roie/gohere/internal/router"
	"github.com/roie/gohere/internal/userpath"
)

type CommandRunner interface {
	Run(ctx context.Context, command string, args ...string) error
}

type DetachedRunner interface {
	StartDetached(ctx context.Context, command string, args ...string) (int, error)
}

type Config struct {
	StateDir         string
	ConfigDir        string
	CurrentBinary    string
	CommandRunner    CommandRunner
	RouterHealth     func(context.Context) error
	Stderr           io.Writer
	Progress         io.Writer
	SystemdAvailable bool
	HTTPS            bool
	TrustCA          func(context.Context, string) error
}

var stopDetachedProcess = func(pid int) {
	if pid <= 0 {
		return
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = process.Kill()
}

func Linux(ctx context.Context, cfg Config) error {
	if cfg.RouterHealth != nil && cfg.RouterHealth(ctx) == nil {
		return nil
	}
	if cfg.StateDir == "" {
		cfg.StateDir = router.DefaultStateDir()
	}
	if cfg.ConfigDir == "" {
		cfg.ConfigDir = filepath.Join(userpath.HomeDir(), ".config")
	}
	if cfg.CurrentBinary == "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		cfg.CurrentBinary = exe
	}
	if cfg.CommandRunner == nil {
		cfg.CommandRunner = realRunner{}
	}
	if cfg.Stderr == nil {
		cfg.Stderr = io.Discard
	}
	if err := enableHTTPSIfRequested(ctx, cfg, "linux"); err != nil {
		return err
	}

	progressf(cfg.Progress, "Installing local service...\n")
	binDir := filepath.Join(cfg.StateDir, "bin")
	if err := os.MkdirAll(binDir, 0700); err != nil {
		return err
	}
	stableBinary := filepath.Join(binDir, "gohere")
	if err := copyFile(cfg.CurrentBinary, stableBinary, 0755); err != nil {
		return err
	}
	if _, err := router.EnsureToken(cfg.StateDir); err != nil {
		return err
	}
	progressf(cfg.Progress, "Allowing local HTTP/HTTPS ports...\n")
	if err := cfg.CommandRunner.Run(ctx, "sudo", "setcap", "cap_net_bind_service=+ep", stableBinary); err != nil {
		return err
	}

	progressf(cfg.Progress, "Starting gohere service...\n")
	if cfg.SystemdAvailable {
		if err := writeSystemdService(cfg.ConfigDir, stableBinary); err != nil {
			return err
		}
		if err := cfg.CommandRunner.Run(ctx, "systemctl", "--user", "enable", "--now", "gohere-router"); err == nil {
			return nil
		} else {
			fmt.Fprintf(cfg.Stderr, "gohere systemd start failed; falling back to detached service: %v\n", err)
		}
	}
	pid, err := startDetached(ctx, cfg.CommandRunner, stableBinary, "service", "run")
	if err != nil {
		return err
	}
	if err := writeDetachedRouterPID(cfg.StateDir, pid); err != nil {
		return err
	}
	return nil
}

func Windows(ctx context.Context, cfg Config) error {
	if cfg.RouterHealth != nil && cfg.RouterHealth(ctx) == nil {
		return nil
	}
	if cfg.StateDir == "" {
		cfg.StateDir = router.DefaultStateDir()
	}
	if cfg.CurrentBinary == "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		cfg.CurrentBinary = exe
	}
	if cfg.CommandRunner == nil {
		cfg.CommandRunner = realRunner{}
	}
	if err := enableHTTPSIfRequested(ctx, cfg, "windows"); err != nil {
		return err
	}

	progressf(cfg.Progress, "Installing local service...\n")
	binDir := filepath.Join(cfg.StateDir, "bin")
	if err := os.MkdirAll(binDir, 0700); err != nil {
		return err
	}
	stableBinary := filepath.Join(binDir, "gohere.exe")
	if err := copyFile(cfg.CurrentBinary, stableBinary, 0755); err != nil {
		return err
	}
	if _, err := router.EnsureToken(cfg.StateDir); err != nil {
		return err
	}
	progressf(cfg.Progress, "Starting gohere service...\n")
	pid, err := startDetached(ctx, cfg.CommandRunner, stableBinary, "service", "run")
	if err != nil {
		return err
	}
	if err := writeDetachedRouterPID(cfg.StateDir, pid); err != nil {
		return err
	}
	return nil
}

func Darwin(ctx context.Context, cfg Config) error {
	if cfg.RouterHealth != nil && cfg.RouterHealth(ctx) == nil {
		return nil
	}
	if cfg.StateDir == "" {
		cfg.StateDir = router.DefaultStateDir()
	}
	if cfg.CurrentBinary == "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		cfg.CurrentBinary = exe
	}
	if cfg.CommandRunner == nil {
		cfg.CommandRunner = realRunner{}
	}
	if err := enableHTTPSIfRequested(ctx, cfg, "darwin"); err != nil {
		return err
	}

	progressf(cfg.Progress, "Installing local service...\n")
	binDir := filepath.Join(cfg.StateDir, "bin")
	if err := os.MkdirAll(binDir, 0700); err != nil {
		return err
	}
	stableBinary := filepath.Join(binDir, "gohere")
	if err := copyFile(cfg.CurrentBinary, stableBinary, 0755); err != nil {
		return err
	}
	if _, err := router.EnsureToken(cfg.StateDir); err != nil {
		return err
	}
	progressf(cfg.Progress, "Starting gohere service...\n")
	pid, err := startDetached(ctx, cfg.CommandRunner, stableBinary, "service", "run")
	if err != nil {
		return err
	}
	if err := writeDetachedRouterPID(cfg.StateDir, pid); err != nil {
		return err
	}
	return nil
}

func enableHTTPSIfRequested(ctx context.Context, cfg Config, goos string) error {
	if !cfg.HTTPS {
		return nil
	}
	progressf(cfg.Progress, "Setting up HTTPS certificates...\n")
	store := localcert.Store{StateDir: cfg.StateDir}
	if _, err := store.EnsureCA(); err != nil {
		return err
	}
	trustCA := cfg.TrustCA
	if trustCA == nil {
		trustCA = func(ctx context.Context, caPath string) error {
			return certtrust.TrustCA(ctx, goos, cfg.CommandRunner, caPath)
		}
	}
	progress(cfg.Progress, trustProgressMessage(goos))
	if err := trustCA(ctx, store.CACertPath()); err != nil {
		return err
	}
	return appconfig.Save(cfg.StateDir, appconfig.Config{HTTPS: true})
}

func progressf(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, format, args...)
}

func progress(w io.Writer, message string) {
	if w == nil {
		return
	}
	fmt.Fprint(w, message)
}

func trustProgressMessage(goos string) string {
	switch goos {
	case "linux":
		return "Trusting certificate authority in Linux...\n"
	case "windows":
		return "Trusting certificate authority in Windows...\n"
	case "darwin":
		return "Trusting certificate authority in macOS...\n"
	default:
		return "Trusting certificate authority...\n"
	}
}

func StartInstalledRouter(ctx context.Context, cfg Config, binaryName string) error {
	if cfg.StateDir == "" {
		cfg.StateDir = router.DefaultStateDir()
	}
	if cfg.CommandRunner == nil {
		cfg.CommandRunner = realRunner{}
	}
	stableBinary := filepath.Join(cfg.StateDir, "bin", binaryName)
	if _, err := os.Stat(stableBinary); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "token")); err != nil {
		return err
	}
	pid, err := startDetached(ctx, cfg.CommandRunner, stableBinary, "service", "run")
	if err != nil {
		return err
	}
	if err := writeDetachedRouterPID(cfg.StateDir, pid); err != nil {
		return err
	}
	return nil
}

func startDetached(ctx context.Context, runner CommandRunner, command string, args ...string) (int, error) {
	if detached, ok := runner.(DetachedRunner); ok {
		return detached.StartDetached(ctx, command, args...)
	}
	if err := runner.Run(ctx, command, args...); err != nil {
		return 0, err
	}
	return 0, nil
}

func writeDetachedRouterPID(stateDir string, pid int) error {
	if err := os.WriteFile(filepath.Join(stateDir, "router.pid"), []byte(strconv.Itoa(pid)+"\n"), 0600); err != nil {
		stopDetachedProcess(pid)
		return err
	}
	return nil
}

func writeSystemdService(configDir, stableBinary string) error {
	serviceDir := filepath.Join(configDir, "systemd", "user")
	if err := os.MkdirAll(serviceDir, 0700); err != nil {
		return err
	}
	service := fmt.Sprintf(`[Unit]
Description=gohere local hostname service

[Service]
ExecStart=%s service run
Restart=on-failure

[Install]
WantedBy=default.target
`, systemdQuote(stableBinary))
	return os.WriteFile(filepath.Join(serviceDir, "gohere-router.service"), []byte(service), 0644)
}

func systemdQuote(value string) string {
	return strconv.Quote(value)
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			os.Remove(tmpPath)
		}
	}()
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := replaceInstalledFile(tmpPath, dst); err != nil {
		return err
	}
	cleanup = false
	return os.Chmod(dst, mode)
}

func replaceInstalledFile(tmpPath, dst string) error {
	return replaceInstalledFileForGOOS(runtime.GOOS, tmpPath, dst, os.Rename)
}

func replaceInstalledFileForGOOS(goos, tmpPath, dst string, rename func(string, string) error) error {
	if goos == "windows" {
		backup, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".*.bak")
		if err != nil {
			return err
		}
		backupPath := backup.Name()
		if err := backup.Close(); err != nil {
			os.Remove(backupPath)
			return err
		}
		if err := os.Remove(backupPath); err != nil && !os.IsNotExist(err) {
			return err
		}

		hasExisting := true
		if err := rename(dst, backupPath); err != nil {
			if !os.IsNotExist(err) {
				return err
			}
			hasExisting = false
		}
		if err := rename(tmpPath, dst); err != nil {
			if hasExisting {
				_ = rename(backupPath, dst)
			}
			return err
		}
		if hasExisting {
			_ = os.Remove(backupPath)
		}
		return nil
	}
	return rename(tmpPath, dst)
}

type realRunner struct{}

func (realRunner) Run(ctx context.Context, command string, args ...string) error {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}
