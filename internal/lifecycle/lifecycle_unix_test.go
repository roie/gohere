//go:build !windows

package lifecycle

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/roie/gohere/internal/router"
)

func TestPIDAlivePermissionDeniedPreservesReachableRoute(t *testing.T) {
	process, err := os.FindProcess(1)
	if err != nil {
		t.Fatal(err)
	}
	defer process.Release()
	if err := process.Signal(syscall.Signal(0)); !errors.Is(err, syscall.EPERM) {
		t.Skipf("requires signal-zero EPERM for PID 1, got %v", err)
	}
	if !PIDAlive(1) {
		t.Error("PIDAlive reported a live process dead after EPERM")
	}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	route := router.Route{
		ID:             "permission-denied",
		Generation:     1,
		State:          router.RouteStateActive,
		Host:           "live.localhost",
		Target:         backend.URL,
		PID:            1,
		LeaseExpiresAt: time.Now().Add(time.Minute),
	}
	statuses := RouteStatuses([]router.Route{route})
	if len(statuses) != 1 || statuses[0].Status != RouteStatusReady {
		t.Errorf("statuses = %#v, want ready", statuses)
	}

	store := router.NewMemoryStore()
	if err := store.Save([]router.Route{route}); err != nil {
		t.Fatal(err)
	}
	removed, err := Prune(store)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Errorf("pruned %d routes, want 0", removed)
	}
	kept, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 || kept[0].Ref() != route.Ref() {
		t.Fatalf("remaining routes = %#v, want original route", kept)
	}
}
