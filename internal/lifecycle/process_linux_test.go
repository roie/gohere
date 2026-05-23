//go:build linux

package lifecycle

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/roie/gohere/internal/router"
)

func TestRouteProcessVerifiedWithRealLinuxProcessStartTime(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	startedAt, ok := realProcessStartTime(cmd.Process.Pid)
	if !ok {
		t.Fatal("expected real process start time")
	}

	if !RouteProcessVerified(router.Route{PID: cmd.Process.Pid, StartedAt: startedAt.Add(time.Second)}) {
		t.Fatal("expected route to verify when recorded after process start")
	}
	if RouteProcessVerified(router.Route{PID: cmd.Process.Pid, StartedAt: startedAt.Add(-time.Second)}) {
		t.Fatal("expected route not to verify when recorded before process start")
	}
}

func TestStopPIDTerminatesProcessGroup(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is required for process group test")
	}
	dir := t.TempDir()
	childPIDPath := filepath.Join(dir, "child.pid")
	cmd := exec.Command("sh", "-c", "sleep 30 & echo $! > "+childPIDPath+"; wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})

	childPID := waitForChildPID(t, childPIDPath)
	StopPID(cmd.Process.Pid)
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("process group leader did not exit after StopPID")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(childPID) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("child process %d is still alive after StopPID", childPID)
}

func waitForChildPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			return pid
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for child pid file %s", path)
	return 0
}

func pidAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
