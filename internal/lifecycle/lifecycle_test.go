package lifecycle

import (
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/roie/gohere/internal/router"
)

func TestFormatRoutesShowsReachability(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()

	out := FormatRoutes([]RouteStatus{{
		Route:     router.Route{Host: "app.localhost", Target: backend.URL, CWD: "/tmp/app", PID: 123},
		Reachable: true,
	}})
	if !strings.Contains(out, "app.localhost") || !strings.Contains(out, "backend yes") {
		t.Fatalf("output = %q", out)
	}
}

func TestCleanRemovesDeadRoutes(t *testing.T) {
	store := router.NewMemoryStore()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()
	store.Save([]router.Route{
		{Host: "live.localhost", Target: backend.URL},
		{Host: "dead.localhost", Target: "http://127.0.0.1:1"},
	})

	removed, err := Clean(store)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	routes, _ := store.Load()
	if len(routes) != 1 || routes[0].Host != "live.localhost" {
		t.Fatalf("routes = %#v", routes)
	}
}

func TestStopCurrentFolderRemovesStaleRouteAndReportsNotStopped(t *testing.T) {
	store := router.NewMemoryStore()
	store.Save([]router.Route{
		{Host: "app.localhost", CWD: "/tmp/app", PID: 999999, StartedAt: time.Now()},
		{Host: "api.localhost", CWD: "/tmp/api", PID: 999998, StartedAt: time.Now()},
	})

	stopped, err := StopCurrent(store, "/tmp/app")
	if err != nil {
		t.Fatal(err)
	}
	if stopped {
		t.Fatal("stale PID should not be reported as stopped")
	}
	routes, _ := store.Load()
	if len(routes) != 1 || routes[0].Host != "api.localhost" {
		t.Fatalf("routes = %#v", routes)
	}
}

func TestStopCurrentFolderStopsLiveProcess(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()

	store := router.NewMemoryStore()
	store.Save([]router.Route{{Host: "app.localhost", CWD: "/tmp/app", PID: cmd.Process.Pid, StartedAt: time.Now()}})

	stopped, err := StopCurrent(store, "/tmp/app")
	if err != nil {
		t.Fatal(err)
	}
	if !stopped {
		t.Fatal("expected live process to be stopped")
	}
	routes, _ := store.Load()
	if len(routes) != 0 {
		t.Fatalf("routes = %#v", routes)
	}
}

func TestStopCurrentReportsMissingRoute(t *testing.T) {
	store := router.NewMemoryStore()
	stopped, err := StopCurrent(store, "/tmp/app")
	if err != nil {
		t.Fatal(err)
	}
	if stopped {
		t.Fatal("expected no route")
	}
}
