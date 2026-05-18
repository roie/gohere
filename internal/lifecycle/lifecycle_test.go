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

func TestFormatRoutesShowsCompactTable(t *testing.T) {
	out := FormatRoutes([]RouteStatus{{
		Route:  router.Route{Host: "app.localhost", Target: "http://127.0.0.1:5173", CWD: "/tmp/app", PID: 123},
		Status: RouteStatusUnknown,
	}})
	if !strings.Contains(out, "host") || !strings.Contains(out, "target") || !strings.Contains(out, "status") {
		t.Fatalf("output = %q", out)
	}
	if !strings.Contains(out, "app.localhost") || !strings.Contains(out, "unknown") || strings.Contains(out, "dead") || strings.Contains(out, "cwd") || strings.Contains(out, "pid") {
		t.Fatalf("output = %q", out)
	}
}

func TestFormatRoutesVerboseShowsOperationalDetails(t *testing.T) {
	out := FormatRoutesVerbose([]RouteStatus{{
		Route:  router.Route{Host: "app.localhost", Target: "http://127.0.0.1:5173", CWD: "/tmp/app", PID: 123},
		Status: RouteStatusReady,
	}})
	if !strings.Contains(out, "host") || !strings.Contains(out, "target") || !strings.Contains(out, "status") || !strings.Contains(out, "pid") || !strings.Contains(out, "cwd") {
		t.Fatalf("output = %q", out)
	}
	if !strings.Contains(out, "app.localhost") || !strings.Contains(out, "ready") || !strings.Contains(out, "123") || !strings.Contains(out, "/tmp/app") {
		t.Fatalf("output = %q", out)
	}
	if strings.Contains(out, "backend") || strings.Contains(out, "cwd /tmp/app") || strings.Contains(out, "pid 123") {
		t.Fatalf("output = %q", out)
	}
}

func TestFormatRoutesUsesSharedStatusSemantics(t *testing.T) {
	statuses := []RouteStatus{
		{Route: router.Route{Host: "ready.localhost", Target: "http://127.0.0.1:5173"}, Status: RouteStatusReady},
		{Route: router.Route{Host: "dead.localhost", Target: "http://127.0.0.1:5174"}, Status: RouteStatusDead},
		{Route: router.Route{Host: "unknown.localhost", Target: "http://127.0.0.1:5175"}, Status: RouteStatusUnknown},
	}

	for _, format := range []func([]RouteStatus) string{FormatRoutes, FormatRoutesVerbose} {
		out := format(statuses)
		for _, want := range []string{"ready", "dead", "unknown"} {
			if !strings.Contains(out, want) {
				t.Fatalf("output missing %q: %q", want, out)
			}
		}
	}
}

func TestCleanRemovesDeadRoutes(t *testing.T) {
	store := router.NewMemoryStore()
	store.Save([]router.Route{
		{Host: "unknown.localhost", Target: "://bad-url"},
		{Host: "dead.localhost", Target: "http://127.0.0.1:5173", PID: 999999},
	})

	removed, err := Clean(store)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	routes, _ := store.Load()
	if len(routes) != 1 || routes[0].Host != "unknown.localhost" {
		t.Fatalf("routes = %#v", routes)
	}
}

func TestCleanRemovesDeadPIDRouteEvenIfTargetReachable(t *testing.T) {
	store := router.NewMemoryStore()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()
	store.Save([]router.Route{{Host: "dead-pid.localhost", Target: backend.URL, PID: 999999}})

	removed, err := Clean(store)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	routes, _ := store.Load()
	if len(routes) != 0 {
		t.Fatalf("routes = %#v", routes)
	}
}

func TestCleanKeepsUnknownRoutes(t *testing.T) {
	store := router.NewMemoryStore()
	store.Save([]router.Route{{Host: "unknown.localhost", Target: "://bad-url"}})

	removed, err := Clean(store)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
	routes, _ := store.Load()
	if len(routes) != 1 || routes[0].Host != "unknown.localhost" {
		t.Fatalf("routes = %#v", routes)
	}
}

func TestStopCurrentFolderRemovesStaleRouteAndReportsNotStopped(t *testing.T) {
	store := router.NewMemoryStore()
	store.Save([]router.Route{
		{Host: "app.localhost", CWD: "/tmp/app", PID: 999999, StartedAt: time.Now()},
		{Host: "api.localhost", CWD: "/tmp/api", PID: 999998, StartedAt: time.Now()},
	})

	host, stopped, err := StopCurrent(store, "/tmp/app")
	if err != nil {
		t.Fatal(err)
	}
	if host != "app.localhost" {
		t.Fatalf("host = %q", host)
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

	host, stopped, err := StopCurrent(store, "/tmp/app")
	if err != nil {
		t.Fatal(err)
	}
	if host != "app.localhost" {
		t.Fatalf("host = %q", host)
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
	host, stopped, err := StopCurrent(store, "/tmp/app")
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		t.Fatalf("host = %q", host)
	}
	if stopped {
		t.Fatal("expected no route")
	}
}
