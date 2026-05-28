//go:build windows

package setup

import (
	"syscall"
	"testing"
)

func TestDetachedSysProcAttrHidesWindowsConsole(t *testing.T) {
	attr := detachedSysProcAttr()
	if attr == nil {
		t.Fatal("detachedSysProcAttr() = nil")
	}
	if !attr.HideWindow {
		t.Fatalf("HideWindow = false, want true")
	}
	for _, flag := range []uint32{syscall.CREATE_NEW_PROCESS_GROUP, windowsDetachedProcess, windowsCreateNoWindow} {
		if attr.CreationFlags&flag == 0 {
			t.Fatalf("CreationFlags = %#x, want flag %#x", attr.CreationFlags, flag)
		}
	}
}
