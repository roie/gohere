package app

import (
	"testing"

	"github.com/roie/gohere/internal/router"
)

func stubRunnerIdentity(t *testing.T) {
	t.Helper()
	original := activationProcess
	activationProcess = func(ref router.RouteRef, pid int) (router.RouteProcess, error) {
		if pid == 0 {
			return router.RouteProcess{Ref: ref, PID: 1, ProcessIdentity: "test-runner"}, nil
		}
		return original(ref, pid)
	}
	t.Cleanup(func() { activationProcess = original })
}
