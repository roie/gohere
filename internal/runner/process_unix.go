//go:build !windows

package runner

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

func configureProcessTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid
	if pid <= 0 {
		return nil
	}
	err := syscall.Kill(-pid, syscall.SIGTERM)
	if err != nil && !errorsIsProcessDone(err) {
		return err
	}
	time.Sleep(500 * time.Millisecond)
	err = syscall.Kill(-pid, syscall.SIGKILL)
	if err != nil && !errorsIsProcessDone(err) {
		return err
	}
	return nil
}

func errorsIsProcessDone(err error) bool {
	return err == nil || err == os.ErrProcessDone || err == syscall.ESRCH
}
