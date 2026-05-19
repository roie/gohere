//go:build !windows

package setup

import "syscall"

func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
