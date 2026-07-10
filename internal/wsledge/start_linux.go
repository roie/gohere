//go:build linux

package wsledge

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

func StartDetached(ctx context.Context, edgeBinary, companionBinary, logPath string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0700); err != nil {
		return 0, err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return 0, err
	}
	defer logFile.Close()
	command := exec.Command(edgeBinary, InternalCommand, companionBinary)
	command.Stdin = nil
	command.Stdout = logFile
	command.Stderr = logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return 0, err
	}
	pid := command.Process.Pid
	return pid, command.Process.Release()
}
