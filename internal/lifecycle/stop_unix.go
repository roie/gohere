//go:build !windows

package lifecycle

import (
	"syscall"
	"time"
)

func stopProcessGroup(pid int) bool {
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		return false
	}
	time.Sleep(500 * time.Millisecond)
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	return true
}
