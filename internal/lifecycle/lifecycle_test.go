package lifecycle

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/roie/gohere/internal/router"
)

func TestFormatRoutesShowsTypedShareValues(t *testing.T) {
	out := FormatRoutes([]RouteStatus{
		{Route: router.Route{Host: "app.localhost", Target: "http://127.0.0.1:5173", LANShare: &router.LANShare{State: router.LANShareActive, Hostname: "app.local."}}, Status: RouteStatusReady},
		{Route: router.Route{Host: "api.localhost", Target: "http://127.0.0.1:4000", LANShare: &router.LANShare{State: router.LANShareSuspended, Hostname: "api.local.", SuspendedReason: "network changed"}}, Status: RouteStatusReady},
		{Route: router.Route{Host: "docs.localhost", Target: "http://127.0.0.1:3000"}, Status: RouteStatusReady},
	})
	header := strings.Split(out, "\n")[0]
	for _, want := range []string{"host", "target", "status", "share", "lan: https://app.local", "lan: unavailable", "docs.localhost", "—"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q: %q", want, out)
		}
	}
	if strings.Contains(header, "lan") || strings.Contains(out, "suspended:") || strings.Contains(out, "network changed") {
		t.Fatalf("output exposes mode as a header or internal state: %q", out)
	}
}

func TestFormatRoutesHidesShareColumnUnlessSharingIsConfigured(t *testing.T) {
	withoutShare := []RouteStatus{{Route: router.Route{Host: "app.localhost", Target: "http://127.0.0.1:5173"}, Status: RouteStatusReady}}
	if out := FormatRoutes(withoutShare); strings.Contains(strings.Split(out, "\n")[0], "share") {
		t.Fatalf("unexpected share column: %q", out)
	}

	withSuspendedShare := append(withoutShare, RouteStatus{Route: router.Route{Host: "api.localhost", Target: "http://127.0.0.1:5174", LANShare: &router.LANShare{State: router.LANShareSuspended, Hostname: "api.local."}}, Status: RouteStatusReady})
	out := FormatRoutes(withSuspendedShare)
	if !strings.Contains(strings.Split(out, "\n")[0], "share") || !strings.Contains(out, "lan: unavailable") {
		t.Fatalf("configured share output = %q", out)
	}
}

func TestFormatRoutesVerboseShowsSummaryBeforeOperationalDetails(t *testing.T) {
	ownerEnv := foreignOwnerEnv()
	out := FormatRoutesVerbose([]RouteStatus{
		{Route: router.Route{Host: "app.localhost", Target: "http://127.0.0.1:5173", CWD: "/tmp/app", PID: 123, Mode: "package", Source: "wsl", OwnerEnv: ownerEnv, StartedAt: time.Date(2026, 5, 28, 1, 2, 3, 0, time.UTC)}, Status: RouteStatusReady},
		{Route: router.Route{Host: "api.localhost", Target: "http://127.0.0.1:4000"}, Status: RouteStatusReady},
	})
	for _, want := range []string{
		"host", "target", "status", "details",
		"  app.localhost", "    cwd      /tmp/app", "    mode     package", "    source   wsl", "    owner    " + ownerEnv, "    pid      123", "    started  2026-05-28T01:02:03Z",
		"  api.localhost", "    mode     —", "    pid      —", "    started  —",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q: %q", want, out)
		}
	}
	detailsAt := strings.Index(out, "\ndetails\n")
	if detailsAt < 0 || strings.Index(out, "api.localhost") > detailsAt {
		t.Fatalf("summary table is not complete before details: %q", out)
	}
	if strings.Contains(out, "stop") || strings.Contains(out, "route belongs to another environment") || strings.Contains(out, "    mode     unknown") {
		t.Fatalf("output contains internal or synthetic metadata: %q", out)
	}
}

func TestFormatRoutesVerboseUsesDashForMissingPID(t *testing.T) {
	out := FormatRoutesVerbose([]RouteStatus{{Route: router.Route{Host: "app.localhost", Target: "http://127.0.0.1:5173"}, Status: RouteStatusReady}})
	if !strings.Contains(out, "    pid      —") || strings.Contains(out, "    pid      0") {
		t.Fatalf("output = %q", out)
	}
}

func foreignOwnerEnv() string {
	if currentOwnerEnv() == "windows" {
		return "wsl"
	}
	return "windows"
}

func TestFormatRoutesUsesSharedStatusSemantics(t *testing.T) {
	statuses := []RouteStatus{
		{Route: router.Route{Host: "starting.localhost", State: router.RouteStatePending, PendingTarget: "http://127.0.0.1:5172"}, Status: RouteStatusStarting},
		{Route: router.Route{Host: "ready.localhost", Target: "http://127.0.0.1:5173"}, Status: RouteStatusReady},
		{Route: router.Route{Host: "dead.localhost", Target: "http://127.0.0.1:5174"}, Status: RouteStatusDead},
		{Route: router.Route{Host: "unknown.localhost", Target: "http://127.0.0.1:5175"}, Status: RouteStatusUnknown},
	}

	for _, format := range []func([]RouteStatus) string{FormatRoutes, FormatRoutesVerbose} {
		out := format(statuses)
		for _, want := range []string{"starting", "ready", "dead", "unknown"} {
			if !strings.Contains(out, want) {
				t.Fatalf("output missing %q: %q", want, out)
			}
		}
	}
}

func TestRouteStatusesReportPendingAsStarting(t *testing.T) {
	statuses := RouteStatusesWithRouterReady([]router.Route{{
		State:         router.RouteStatePending,
		Host:          "web.localhost",
		PendingTarget: "http://127.0.0.1:5173",
	}}, true)
	if len(statuses) != 1 || statuses[0].Status != RouteStatusStarting {
		t.Fatalf("statuses = %#v, want starting", statuses)
	}
}

func TestRouteStatusesMarkReachableExpiredNativeRouteUnknown(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()
	statuses := RouteStatusesWithRouterReady([]router.Route{{
		State:          router.RouteStateActive,
		Host:           "web.localhost",
		Target:         backend.URL,
		LeaseExpiresAt: time.Now().Add(-time.Second),
	}}, true)
	if len(statuses) != 1 || statuses[0].Status != RouteStatusUnknown {
		t.Fatalf("statuses = %#v, want unknown", statuses)
	}
}

func TestRouteStatusesAreUnknownWhenRouterIsNotReady(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()

	statuses := RouteStatusesWithRouterReady([]router.Route{{
		Host:   "web.localhost",
		Target: backend.URL,
	}}, false)

	if len(statuses) != 1 || statuses[0].Status != RouteStatusUnknown {
		t.Fatalf("statuses = %#v, want unknown", statuses)
	}
}

func TestRouteStatusUsesHEADWithoutGET(t *testing.T) {
	var methods []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.Method == http.MethodGet {
			t.Fatal("route status probe should not use GET")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()

	statuses := RouteStatuses([]router.Route{{
		Host:   "web.localhost",
		Target: backend.URL,
	}})

	if len(statuses) != 1 || statuses[0].Status != RouteStatusReady {
		t.Fatalf("statuses = %#v, want ready", statuses)
	}
	if len(methods) != 1 || methods[0] != http.MethodHead {
		t.Fatalf("methods = %#v, want HEAD only", methods)
	}
}

func TestRouteStatusTreatsRedirectAsReadyWithoutFollowing(t *testing.T) {
	var methods []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		http.Redirect(w, r, "/login", http.StatusFound)
	}))
	defer backend.Close()

	statuses := RouteStatuses([]router.Route{{
		Host:   "web.localhost",
		Target: backend.URL,
	}})

	if len(statuses) != 1 || statuses[0].Status != RouteStatusReady {
		t.Fatalf("statuses = %#v, want ready", statuses)
	}
	if len(methods) != 1 || methods[0] != "HEAD /" {
		t.Fatalf("methods = %#v, want no redirect follow", methods)
	}
}

func TestRouteStatusUsesSharedProbeTimeout(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()

	statuses := RouteStatuses([]router.Route{{
		Host:   "slow.localhost",
		Target: backend.URL,
	}})

	if len(statuses) != 1 || statuses[0].Status != RouteStatusReady {
		t.Fatalf("statuses = %#v, want ready for slow-but-alive target", statuses)
	}
}

func TestPruneRemovesDeadRoutes(t *testing.T) {
	store := router.NewMemoryStore()
	store.Save([]router.Route{
		{Host: "unknown.localhost", Target: "://bad-url"},
		{Host: "dead.localhost", Target: "http://127.0.0.1:5173", PID: 999999},
	})

	removed, err := Prune(store)
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

func TestPruneRemovesExpiredPendingAndActiveLeasesWithoutRouter(t *testing.T) {
	now := time.Now().UTC()
	store := router.NewMemoryStore()
	if err := store.Save([]router.Route{
		{ID: "pending", Generation: 1, Host: "pending.localhost", State: router.RouteStatePending, ReservationExpiresAt: now.Add(-time.Minute)},
		{ID: "active", Generation: 1, Host: "active.localhost", State: router.RouteStateActive, Target: "http://127.0.0.1:1", LeaseExpiresAt: now.Add(-time.Minute)},
	}); err != nil {
		t.Fatal(err)
	}
	removed, err := PruneWithRouterReady(store, false)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	routes, _ := store.Load()
	if len(routes) != 0 {
		t.Fatalf("expired routes remain: %#v", routes)
	}
}

func TestPrunePreservesReplacementGenerationAddedDuringUpdate(t *testing.T) {
	old := router.Route{ID: "route", Generation: 1, Host: "app.localhost", Target: "http://127.0.0.1:5173", PID: 999999}
	replacement := old
	replacement.Generation = 2
	replacement.PID = 0
	store := &interleavingRouteStore{routes: []router.Route{old}, added: replacement}
	removed, err := PruneWithRouterReady(store, true)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want old generation only", removed)
	}
	routes, _ := store.Load()
	if len(routes) != 1 || routes[0].Ref() != replacement.Ref() {
		t.Fatalf("replacement was pruned: %#v", routes)
	}
}

func TestPrunePreservesRoutesAddedBetweenLoadAndSave(t *testing.T) {
	added := router.Route{Host: "added.localhost", Target: "://bad-url"}
	store := &interleavingRouteStore{
		routes: []router.Route{{Host: "dead.localhost", Target: "http://127.0.0.1:5173", PID: 999999}},
		added:  added,
	}

	removed, err := PruneWithRouterReady(store, true)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if !store.usedUpdate {
		t.Fatal("prune did not use transactional store update")
	}
	routes, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0].Host != "added.localhost" {
		t.Fatalf("routes = %#v, want concurrently added route preserved", routes)
	}
}

func TestPruneKeepsRoutesWhenRouterIsNotReady(t *testing.T) {
	store := router.NewMemoryStore()
	store.Save([]router.Route{{Host: "dead.localhost", Target: "http://127.0.0.1:1", PID: 999999}})

	removed, err := PruneWithRouterReady(store, false)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
	routes, _ := store.Load()
	if len(routes) != 1 || routes[0].Host != "dead.localhost" {
		t.Fatalf("routes = %#v", routes)
	}
}

func TestPruneRemovesDeadPIDRouteEvenIfTargetReachable(t *testing.T) {
	store := router.NewMemoryStore()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()
	store.Save([]router.Route{{Host: "dead-pid.localhost", Target: backend.URL, PID: 999999}})

	removed, err := Prune(store)
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

func TestRouteStatusesDoNotUseWSLPIDOutsideWSL(t *testing.T) {
	oldCurrentIsWSL := currentIsWSL
	defer func() {
		currentIsWSL = oldCurrentIsWSL
	}()
	currentIsWSL = func() bool { return false }
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()

	statuses := RouteStatuses([]router.Route{{
		Host:   "web.localhost",
		Target: backend.URL,
		PID:    999999,
		Source: "wsl",
	}})

	if len(statuses) != 1 || statuses[0].Status != RouteStatusReady {
		t.Fatalf("statuses = %#v, want ready", statuses)
	}
}

type interleavingRouteStore struct {
	routes     []router.Route
	added      router.Route
	injected   bool
	usedUpdate bool
}

func (s *interleavingRouteStore) Load() ([]router.Route, error) {
	routes := cloneTestRoutes(s.routes)
	if !s.injected {
		s.injected = true
		s.routes = append(s.routes, s.added)
	}
	return routes, nil
}

func (s *interleavingRouteStore) Save(routes []router.Route) error {
	s.routes = cloneTestRoutes(routes)
	return nil
}

func (s *interleavingRouteStore) Update(fn func([]router.Route) ([]router.Route, error)) error {
	s.usedUpdate = true
	next, err := fn(cloneTestRoutes(s.routes))
	if err != nil {
		return err
	}
	s.routes = cloneTestRoutes(next)
	return nil
}

func cloneTestRoutes(routes []router.Route) []router.Route {
	return append([]router.Route(nil), routes...)
}

func TestRouteStatusesDoNotUseWindowsPIDInsideWSL(t *testing.T) {
	oldCurrentIsWSL := currentIsWSL
	defer func() {
		currentIsWSL = oldCurrentIsWSL
	}()
	currentIsWSL = func() bool { return true }

	statuses := RouteStatuses([]router.Route{{
		Host:     "windows.localhost",
		Target:   "://bad-target",
		PID:      999999,
		OwnerEnv: "windows",
	}})

	if len(statuses) != 1 {
		t.Fatalf("statuses = %#v", statuses)
	}
	if statuses[0].Status == RouteStatusDead {
		t.Fatalf("status = %s, want not dead from WSL PID namespace", statuses[0].Status)
	}
}

func TestPruneKeepsReachableWSLRouteWithForeignPID(t *testing.T) {
	oldCurrentIsWSL := currentIsWSL
	defer func() {
		currentIsWSL = oldCurrentIsWSL
	}()
	currentIsWSL = func() bool { return false }
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()
	store := router.NewMemoryStore()
	store.Save([]router.Route{{Host: "web.localhost", Target: backend.URL, PID: 999999, OwnerEnv: "wsl"}})

	removed, err := Prune(store)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
	routes, _ := store.Load()
	if len(routes) != 1 || routes[0].Host != "web.localhost" {
		t.Fatalf("routes = %#v", routes)
	}
}

func TestPruneKeepsUnknownRoutes(t *testing.T) {
	store := router.NewMemoryStore()
	store.Save([]router.Route{{Host: "unknown.localhost", Target: "://bad-url"}})

	removed, err := Prune(store)
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

func TestStopCWDsPreservesReplacementGenerationAddedDuringUpdate(t *testing.T) {
	cwd := t.TempDir()
	old := router.Route{ID: "route", Generation: 1, Host: "app.localhost", Target: "http://127.0.0.1:1", CWD: cwd, PID: 999999}
	replacement := old
	replacement.Generation = 2
	store := &interleavingRouteStore{routes: []router.Route{old}, added: replacement}
	if _, err := StopCWDs(store, []string{cwd}); err != nil {
		t.Fatal(err)
	}
	routes, _ := store.Load()
	if len(routes) != 1 || routes[0].Ref() != replacement.Ref() {
		t.Fatalf("replacement removed by stale stop: %#v", routes)
	}
}

func TestStopCurrentFolderRemovesStaleRouteAndReportsNotStopped(t *testing.T) {
	store := router.NewMemoryStore()
	store.Save([]router.Route{
		{Host: "app.localhost", CWD: "/tmp/app", PID: 999999, StartedAt: time.Now()},
		{Host: "api.localhost", CWD: "/tmp/api", PID: 999998, StartedAt: time.Now()},
	})

	host, stopped, warning, err := StopCurrent(store, "/tmp/app")
	if err != nil {
		t.Fatal(err)
	}
	if host != "app.localhost" {
		t.Fatalf("host = %q", host)
	}
	if stopped {
		t.Fatal("stale PID should not be reported as stopped")
	}
	if warning != "" {
		t.Fatalf("warning = %q", warning)
	}
	routes, _ := store.Load()
	if len(routes) != 1 || routes[0].Host != "api.localhost" {
		t.Fatalf("routes = %#v", routes)
	}
}

func TestStopCurrentFolderMatchesOwnerCWD(t *testing.T) {
	store := router.NewMemoryStore()
	store.Save([]router.Route{
		{Host: "app.localhost", CWD: "/tmp/app/components", OwnerCWD: "/tmp/app", PID: 999999, StartedAt: time.Now()},
		{Host: "api.localhost", CWD: "/tmp/api", OwnerCWD: "/tmp/api", PID: 999998, StartedAt: time.Now()},
	})

	host, stopped, warning, err := StopCurrent(store, "/tmp/app")
	if err != nil {
		t.Fatal(err)
	}
	if host != "app.localhost" {
		t.Fatalf("host = %q", host)
	}
	if stopped {
		t.Fatal("stale PID should not be reported as stopped")
	}
	if warning != "" {
		t.Fatalf("warning = %q", warning)
	}
	routes, _ := store.Load()
	if len(routes) != 1 || routes[0].Host != "api.localhost" {
		t.Fatalf("routes = %#v", routes)
	}
}

func TestStopCurrentFolderDoesNotStopLiveProcessWithoutIdentityVerification(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()

	store := router.NewMemoryStore()
	store.Save([]router.Route{{Host: "app.localhost", CWD: "/tmp/app", PID: cmd.Process.Pid}})

	host, stopped, warning, err := StopCurrent(store, "/tmp/app")
	if err != nil {
		t.Fatal(err)
	}
	if host != "app.localhost" {
		t.Fatalf("host = %q", host)
	}
	if stopped {
		t.Fatal("PID-only process should not be stopped")
	}
	if !strings.Contains(warning, "Could not verify the original gohere process. Not stopping PID") {
		t.Fatalf("warning = %q", warning)
	}
	routes, _ := store.Load()
	if len(routes) != 1 {
		t.Fatalf("routes = %#v", routes)
	}
}

func TestStopCurrentFolderStopsVerifiedLiveProcess(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()

	oldProcessStartTime := processStartTime
	oldStopPID := stopPID
	stoppedPID := 0
	t.Cleanup(func() {
		processStartTime = oldProcessStartTime
		stopPID = oldStopPID
	})
	processStartTime = func(pid int) (time.Time, bool) {
		if pid != cmd.Process.Pid {
			return time.Time{}, false
		}
		return time.Now().Add(-time.Second), true
	}
	stopPID = func(pid int) {
		stoppedPID = pid
	}

	store := router.NewMemoryStore()
	store.Save([]router.Route{{Host: "app.localhost", CWD: "/tmp/app", PID: cmd.Process.Pid, StartedAt: time.Now()}})

	host, stopped, warning, err := StopCurrent(store, "/tmp/app")
	if err != nil {
		t.Fatal(err)
	}
	if host != "app.localhost" {
		t.Fatalf("host = %q", host)
	}
	if !stopped {
		t.Fatal("expected verified process to be stopped")
	}
	if warning != "" {
		t.Fatalf("warning = %q", warning)
	}
	if stoppedPID != cmd.Process.Pid {
		t.Fatalf("stopped PID = %d, want %d", stoppedPID, cmd.Process.Pid)
	}
	routes, _ := store.Load()
	if len(routes) != 0 {
		t.Fatalf("routes = %#v", routes)
	}
}

func TestRouteProcessVerifiedUsesProcessIdentity(t *testing.T) {
	oldProcessIdentity := processIdentity
	t.Cleanup(func() {
		processIdentity = oldProcessIdentity
	})
	processIdentity = func(pid int) (string, bool) {
		if pid != 123 {
			return "", false
		}
		return "linux:456", true
	}

	route := router.Route{
		PID:             123,
		StartedAt:       time.Now().Add(-time.Hour),
		ProcessIdentity: "linux:456",
	}
	if !RouteProcessVerified(route) {
		t.Fatal("expected matching process identity to verify")
	}

	route.StartedAt = time.Time{}
	if !RouteProcessVerified(route) {
		t.Fatal("expected matching process identity to verify without startedAt")
	}

	route.ProcessIdentity = "linux:789"
	if RouteProcessVerified(route) {
		t.Fatal("expected mismatched process identity to be rejected")
	}
}

func TestTasklistContainsPIDScansAllCSVFields(t *testing.T) {
	output := `"gohere.exe","Console","12345","10,000 K"` + "\n"

	if !tasklistContainsPID(output, 12345) {
		t.Fatal("expected tasklist parser to find PID outside second column")
	}
}

func TestStopCurrentFolderRemovesDeadRouteWithoutStoppingUnverifiedPID(t *testing.T) {
	store := router.NewMemoryStore()
	store.Save([]router.Route{{
		Host:      "app.localhost",
		CWD:       "/tmp/app",
		PID:       999999,
		Target:    "http://127.0.0.1:1",
		StartedAt: time.Now(),
	}})

	host, stopped, warning, err := StopCurrent(store, "/tmp/app")
	if err != nil {
		t.Fatal(err)
	}
	if host != "app.localhost" {
		t.Fatalf("host = %q", host)
	}
	if stopped {
		t.Fatal("dead unverified route should not report a stopped process")
	}
	if warning != "" {
		t.Fatalf("warning = %q", warning)
	}
	routes, _ := store.Load()
	if len(routes) != 0 {
		t.Fatalf("routes = %#v", routes)
	}
}

func TestStopCurrentReportsMissingRoute(t *testing.T) {
	store := router.NewMemoryStore()
	host, stopped, warning, err := StopCurrent(store, "/tmp/app")
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		t.Fatalf("host = %q", host)
	}
	if stopped {
		t.Fatal("expected no route")
	}
	if warning != "" {
		t.Fatalf("warning = %q", warning)
	}
}

func TestFormatDoctorShowsHintsForFailedChecks(t *testing.T) {
	out := FormatDoctor([]DoctorCheck{{
		Name:   "port 80",
		OK:     false,
		Detail: "blocked",
		Hint:   "Try: gohere doctor",
	}})

	if !strings.Contains(out, "fail port 80 blocked\n  Try: gohere doctor\n") {
		t.Fatalf("output = %q", out)
	}
}

func TestTasklistContainsPID(t *testing.T) {
	output := `"node.exe","26312","Console","1","30,000 K"` + "\r\n"

	if !tasklistContainsPID(output, 26312) {
		t.Fatal("expected tasklist output to match PID")
	}
	if tasklistContainsPID(output, 26313) {
		t.Fatal("did not expect tasklist output to match another PID")
	}
}

func TestTasklistContainsPIDRejectsNoTasksOutput(t *testing.T) {
	output := "INFO: No tasks are running which match the specified criteria.\r\n"

	if tasklistContainsPID(output, 26312) {
		t.Fatal("did not expect no-tasks output to match PID")
	}
}

func TestTasklistContainsPIDHandlesCommaSeparatedMemory(t *testing.T) {
	pid := 26312
	output := `"node.exe","` + strconv.Itoa(pid) + `","Console","1","123,456 K"` + "\r\n"

	if !tasklistContainsPID(output, pid) {
		t.Fatal("expected CSV parsing to ignore comma inside memory field")
	}
}

func TestParseWindowsProcessStartTime(t *testing.T) {
	startedAt, ok := parseWindowsProcessStartTime("2026-05-23T21:15:42.1234567Z\r\n")
	if !ok {
		t.Fatal("expected start time to parse")
	}
	if startedAt.Year() != 2026 || startedAt.Month() != time.May || startedAt.Day() != 23 {
		t.Fatalf("startedAt = %s", startedAt)
	}
}

func TestParseWindowsProcessStartTimeRejectsInvalidOutput(t *testing.T) {
	if _, ok := parseWindowsProcessStartTime("Saturday, May 23, 2026"); ok {
		t.Fatal("expected localized output to be rejected")
	}
}

func TestParseDarwinProcessStartTime(t *testing.T) {
	startedAt, ok := parseDarwinProcessStartTime("Thu May 28 21:37:17 2026\n", time.UTC)
	if !ok {
		t.Fatal("expected Darwin start time to parse")
	}
	if got := startedAt.Format(time.RFC3339); got != "2026-05-28T21:37:17Z" {
		t.Fatalf("startedAt = %s", got)
	}
}

func TestDarwinProcessStartTimeUsesBoundedCommand(t *testing.T) {
	oldCommandOutput := commandOutput
	defer func() {
		commandOutput = oldCommandOutput
	}()
	commandOutput = func(timeout time.Duration, name string, args ...string) ([]byte, error) {
		if timeout != processCommandTimeout {
			t.Fatalf("timeout = %s, want %s", timeout, processCommandTimeout)
		}
		if name != "ps" {
			t.Fatalf("command = %q, want ps", name)
		}
		want := []string{"-o", "lstart=", "-p", "26312"}
		if len(args) != len(want) {
			t.Fatalf("args = %#v, want %#v", args, want)
		}
		for i := range want {
			if args[i] != want[i] {
				t.Fatalf("args = %#v, want %#v", args, want)
			}
		}
		return []byte("Thu May 28 21:37:17 2026\n"), nil
	}

	startedAt, ok := darwinProcessStartTime(26312)
	if !ok {
		t.Fatal("expected Darwin process start time")
	}
	if startedAt.IsZero() {
		t.Fatal("startedAt is zero")
	}
}

func TestWindowsPIDAliveUsesBoundedCommand(t *testing.T) {
	oldCommandOutput := commandOutput
	defer func() {
		commandOutput = oldCommandOutput
	}()
	var gotTimeout time.Duration
	var gotName string
	commandOutput = func(timeout time.Duration, name string, args ...string) ([]byte, error) {
		gotTimeout = timeout
		gotName = name
		return []byte(`"node.exe","26312","Console","1","30,000 K"`), nil
	}

	if !windowsPIDAlive(26312) {
		t.Fatal("expected PID to be alive")
	}
	if gotName != "tasklist" {
		t.Fatalf("command = %q, want tasklist", gotName)
	}
	if gotTimeout <= 0 {
		t.Fatalf("timeout = %s, want bounded timeout", gotTimeout)
	}
}

func TestWindowsProcessStartTimeUsesBoundedCommand(t *testing.T) {
	oldCommandOutput := commandOutput
	defer func() {
		commandOutput = oldCommandOutput
	}()
	var gotTimeout time.Duration
	var gotName string
	commandOutput = func(timeout time.Duration, name string, args ...string) ([]byte, error) {
		gotTimeout = timeout
		gotName = name
		return []byte("2026-05-23T21:15:42.1234567Z\r\n"), nil
	}

	if _, ok := windowsProcessStartTime(26312); !ok {
		t.Fatal("expected start time")
	}
	if gotName != "powershell.exe" {
		t.Fatalf("command = %q, want powershell.exe", gotName)
	}
	if gotTimeout <= 0 {
		t.Fatalf("timeout = %s, want bounded timeout", gotTimeout)
	}
}

func TestLinuxClockTicksUsesBoundedCommand(t *testing.T) {
	oldCommandOutput := commandOutput
	defer func() {
		commandOutput = oldCommandOutput
	}()
	var gotTimeout time.Duration
	var gotName string
	commandOutput = func(timeout time.Duration, name string, args ...string) ([]byte, error) {
		gotTimeout = timeout
		gotName = name
		return []byte("250\n"), nil
	}

	if ticks := linuxClockTicks(); ticks != 250 {
		t.Fatalf("ticks = %d, want 250", ticks)
	}
	if gotName != "getconf" {
		t.Fatalf("command = %q, want getconf", gotName)
	}
	if gotTimeout <= 0 {
		t.Fatalf("timeout = %s, want bounded timeout", gotTimeout)
	}
}

func TestLinuxClockTicksFallsBackWhenBoundedCommandFails(t *testing.T) {
	oldCommandOutput := commandOutput
	defer func() {
		commandOutput = oldCommandOutput
	}()
	commandOutput = func(timeout time.Duration, name string, args ...string) ([]byte, error) {
		return nil, errors.New("timeout")
	}

	if ticks := linuxClockTicks(); ticks != 100 {
		t.Fatalf("ticks = %d, want fallback 100", ticks)
	}
}
