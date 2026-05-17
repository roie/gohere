package setup

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/roie/gohere/internal/router"
)

type CommandRunner interface {
	Run(ctx context.Context, command string, args ...string) error
}

type Config struct {
	StateDir         string
	ConfigDir        string
	CurrentBinary    string
	CommandRunner    CommandRunner
	SystemdAvailable bool
}

func Linux(ctx context.Context, cfg Config) error {
	if cfg.StateDir == "" {
		cfg.StateDir = router.DefaultStateDir()
	}
	if cfg.ConfigDir == "" {
		cfg.ConfigDir = filepath.Join(homeDir(), ".config")
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
	if err := cfg.CommandRunner.Run(ctx, "sudo", "setcap", "cap_net_bind_service=+ep", stableBinary); err != nil {
		return err
	}

	if cfg.SystemdAvailable {
		if err := writeSystemdService(cfg.ConfigDir, stableBinary); err != nil {
			return err
		}
		return cfg.CommandRunner.Run(ctx, "systemctl", "--user", "enable", "--now", "gohere-router")
	}
	return cfg.CommandRunner.Run(ctx, stableBinary, "router")
}

func writeSystemdService(configDir, stableBinary string) error {
	serviceDir := filepath.Join(configDir, "systemd", "user")
	if err := os.MkdirAll(serviceDir, 0700); err != nil {
		return err
	}
	service := fmt.Sprintf(`[Unit]
Description=gohere local hostname router

[Service]
ExecStart=%s router
Restart=on-failure

[Install]
WantedBy=default.target
`, stableBinary)
	return os.WriteFile(filepath.Join(serviceDir, "gohere-router.service"), []byte(service), 0644)
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

type realRunner struct{}

func (realRunner) Run(ctx context.Context, command string, args ...string) error {
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
