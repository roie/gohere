//go:build linux

package lifecycle

import (
	"os/exec"
	"testing"
	"time"

	"github.com/roie/gohere/internal/router"
)

func TestRouteProcessVerifiedWithRealLinuxProcessStartTime(t *testing.T) {
	beforeStart := time.Now()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	afterStart := time.Now().Add(100 * time.Millisecond)

	if !RouteProcessVerified(router.Route{PID: cmd.Process.Pid, StartedAt: afterStart}) {
		t.Fatal("expected route to verify when recorded after process start")
	}
	if RouteProcessVerified(router.Route{PID: cmd.Process.Pid, StartedAt: beforeStart.Add(-time.Second)}) {
		t.Fatal("expected route not to verify when recorded before process start")
	}
}
