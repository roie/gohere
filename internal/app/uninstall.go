package app

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/roie/gohere/internal/router"
	"github.com/roie/gohere/internal/setup"
)

type UninstallConfig struct {
	StateDir      string
	ConfigDir     string
	CommandRunner setup.CommandRunner
	ProcessSignal func(int) error
}

func Uninstall(ctx context.Context, stdout io.Writer) error {
	return UninstallWithConfig(ctx, stdout, UninstallConfig{})
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
		_ = cfg.ProcessSignal(pid)
	}
	if err := os.Remove(filepath.Join(cfg.StateDir, "bin", "gohere")); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(pidPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Fprintln(stdout, "gohere router removed.")

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

func signalProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Signal(syscall.SIGTERM)
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
