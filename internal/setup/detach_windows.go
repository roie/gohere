//go:build windows

package setup

import "syscall"

const (
	windowsDetachedProcess = 0x00000008
	windowsCreateNoWindow  = 0x08000000
)

func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | windowsDetachedProcess | windowsCreateNoWindow,
		HideWindow:    true,
	}
}
