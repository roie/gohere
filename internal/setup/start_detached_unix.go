//go:build !windows

package setup

import (
	"context"
	"os/exec"
)

func (realRunner) StartDetached(ctx context.Context, command string, args ...string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	cmd := exec.Command(command, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	cmd.SysProcAttr = detachedSysProcAttr()
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	return cmd.Process.Pid, cmd.Process.Release()
}
