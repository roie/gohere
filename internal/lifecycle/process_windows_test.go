//go:build windows

package lifecycle

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestWindowsProcessIdentityIsStableForLiveProcess(t *testing.T) {
	first, ok := ProcessIdentity(os.Getpid())
	if !ok || first == "" {
		t.Fatal("current process identity unavailable")
	}
	second, ok := ProcessIdentity(os.Getpid())
	if !ok || second != first {
		t.Fatalf("identity changed: %q then %q", first, second)
	}
	startedAt, ok := windowsProcessStartTime(os.Getpid())
	if !ok || startedAt.After(time.Now()) || time.Since(startedAt) > time.Hour {
		t.Fatalf("start time = %s, %v", startedAt, ok)
	}
}

func TestWindowsProcessIdentityRejectsExitedProcess(t *testing.T) {
	cmd := exec.Command(os.Getenv("ComSpec"), "/c", "exit", "0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if identity, ok := ProcessIdentity(pid); ok {
		t.Fatalf("exited process identity = %q", identity)
	}
}
