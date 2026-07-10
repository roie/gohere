//go:build windows

package setup

import (
	"strings"
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

func TestWindowsStartProcessScriptQuotesCommandAndArgs(t *testing.T) {
	script := windowsStartProcessScript(`C:\Users\Alice\go'here.exe`, []string{"service", "run"})
	if !strings.Contains(script, "-FilePath 'C:\\Users\\Alice\\go''here.exe'") {
		t.Fatalf("script = %q", script)
	}
	if !strings.Contains(script, "-ArgumentList @('service','run')") {
		t.Fatalf("script = %q", script)
	}
	if !strings.Contains(script, "-PassThru; $p.Id") {
		t.Fatalf("script = %q", script)
	}
}
