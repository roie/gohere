package router

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestLegacyRouteJSONIsActive(t *testing.T) {
	var route Route
	if err := json.Unmarshal([]byte(`{"host":"legacy.localhost","target":"http://127.0.0.1:41000"}`), &route); err != nil {
		t.Fatal(err)
	}
	if got := route.EffectiveState(); got != RouteStateActive {
		t.Fatalf("EffectiveState() = %q, want %q", got, RouteStateActive)
	}
	if route.State != "" {
		t.Fatalf("legacy State = %q, want blank migration state", route.State)
	}
}

func TestFirstLockedMutationAssignsLegacyRouteIdentity(t *testing.T) {
	store := NewMemoryStore()
	if err := store.Save([]Route{{Host: "legacy.localhost", Target: "http://127.0.0.1:41000"}}); err != nil {
		t.Fatal(err)
	}
	if err := UpdateStore(store, func(routes []Route) ([]Route, error) { return routes, nil }); err != nil {
		t.Fatal(err)
	}
	routes, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0].ID == "" || routes[0].Generation != 1 {
		t.Fatalf("legacy route identity = %#v", routes)
	}
}

func TestRouteLifecycleFieldsRoundTrip(t *testing.T) {
	expiresAt := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	want := Route{ID: "route-1", Generation: 3, RunID: "run-1", State: RouteStatePending, Service: "web", PreferredScheme: "https", Host: "web.localhost", PendingTarget: "http://127.0.0.1:41000", ReservationExpiresAt: expiresAt, CWD: "/work/web"}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got Route
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip = %#v, want %#v", got, want)
	}
}

func TestReserveRoutesAtomicallyResolvesConcurrentHostnameRequests(t *testing.T) {
	store := NewMemoryStore()
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	start := make(chan struct{})
	results := make(chan ReservationResult, 2)
	errs := make(chan error, 2)
	for i, target := range []string{"http://127.0.0.1:41001", "http://127.0.0.1:41002"} {
		runID := "run-" + string(rune('a'+i))
		go func() {
			<-start
			result, err := ReserveRoutes(store, ReservationRequest{RunID: runID, TTL: time.Minute, Routes: []RouteReservation{{DesiredHost: "app.localhost", Target: target, CWD: "/work/app"}}}, now)
			results <- result
			errs <- err
		}()
	}
	close(start)
	first, second := <-results, <-results
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if first.Routes[0].Route.Host == second.Routes[0].Route.Host {
		t.Fatalf("concurrent reservations share host %q", first.Routes[0].Route.Host)
	}
	routes, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 2 || routes[0].EffectiveState() != RouteStatePending || routes[1].EffectiveState() != RouteStatePending {
		t.Fatalf("routes = %#v", routes)
	}
}

func TestReserveRoutesBatchFailureDoesNotMutateStore(t *testing.T) {
	store := NewMemoryStore()
	now := time.Now().UTC()
	_, err := ReserveRoutes(store, ReservationRequest{RunID: "run-a", TTL: time.Minute, Routes: []RouteReservation{
		{DesiredHost: "web.localhost", Target: "http://127.0.0.1:42001", CWD: "/work/web"},
		{DesiredHost: "api.localhost", Target: "not-a-url", CWD: "/work/api"},
	}}, now)
	if err == nil {
		t.Fatal("expected invalid target error")
	}
	routes, loadErr := store.Load()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(routes) != 0 {
		t.Fatalf("routes after failed batch = %#v", routes)
	}
}

func TestReserveRoutesReusesOnlyMatchingPreprobedRef(t *testing.T) {
	store := NewMemoryStore()
	existing := Route{ID: "route-1", Generation: 4, State: RouteStateActive, Host: "web.localhost", Target: "http://127.0.0.1:43001", CWD: "/work/web", ProjectRoot: "/work", ProjectName: "work"}
	if err := store.Save([]Route{existing}); err != nil {
		t.Fatal(err)
	}
	ref := existing.Ref()
	result, err := ReserveRoutes(store, ReservationRequest{RunID: "run-b", TTL: time.Minute, Routes: []RouteReservation{{DesiredHost: "web.localhost", Target: "http://127.0.0.1:43002", CWD: "/work/web", ProjectRoot: "/work", ProjectName: "work", Reuse: &ref}}}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Routes[0].Reused || !reflect.DeepEqual(result.Routes[0].Route, existing) {
		t.Fatalf("result = %#v", result)
	}
	badRef := RouteRef{ID: ref.ID, Generation: ref.Generation + 1}
	_, err = ReserveRoutes(store, ReservationRequest{RunID: "run-c", TTL: time.Minute, Routes: []RouteReservation{{DesiredHost: "web.localhost", Target: "http://127.0.0.1:43003", CWD: "/work/web", Reuse: &badRef}}}, time.Now().UTC())
	if !errors.Is(err, ErrRouteRefMismatch) {
		t.Fatalf("error = %v, want ErrRouteRefMismatch", err)
	}
}

func TestReserveRoutesRejectsSameSocketAcrossSchemes(t *testing.T) {
	store := NewMemoryStore()
	now := time.Now().UTC()
	_, err := ReserveRoutes(store, ReservationRequest{RunID: "run-a", TTL: time.Minute, Routes: []RouteReservation{
		{DesiredHost: "web.localhost", Target: "http://127.0.0.1:43500", CWD: "/work/web"},
		{DesiredHost: "api.localhost", Target: "https://127.0.0.1:43500", CWD: "/work/api"},
	}}, now)
	if !errors.Is(err, ErrReservationConflict) {
		t.Fatalf("error = %v, want ErrReservationConflict", err)
	}
	routes, _ := store.Load()
	if len(routes) != 0 {
		t.Fatalf("routes after conflicting batch = %#v", routes)
	}
}

func TestReservationResultPendingRefsExcludesReusedRoutes(t *testing.T) {
	result := ReservationResult{Routes: []ReservedRoute{
		{Route: Route{ID: "new", Generation: 1, State: RouteStatePending}},
		{Route: Route{ID: "existing", Generation: 4, State: RouteStateActive}, Reused: true},
	}}
	got := result.PendingRefs()
	want := []RouteRef{{ID: "new", Generation: 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PendingRefs() = %#v, want %#v", got, want)
	}
}

func TestReserveRoutesReclaimsExpiredPendingRoute(t *testing.T) {
	store := NewMemoryStore()
	now := time.Now().UTC()
	expired := Route{ID: "expired", Generation: 1, RunID: "old", State: RouteStatePending, Host: "app.localhost", PendingTarget: "http://127.0.0.1:44001", ReservationExpiresAt: now.Add(-time.Second), CWD: "/old"}
	if err := store.Save([]Route{expired}); err != nil {
		t.Fatal(err)
	}
	result := reserveTestRun(t, store, "new", "app.localhost", "http://127.0.0.1:44001", "/new", now)
	if result.Routes[0].Route.Host != "app.localhost" {
		t.Fatalf("host = %q", result.Routes[0].Route.Host)
	}
}

func TestReserveRoutesReclaimsExpiredActiveLease(t *testing.T) {
	store := NewMemoryStore()
	now := time.Now().UTC()
	expired := Route{ID: "expired-active", Generation: 2, RunID: "old", State: RouteStateActive, Host: "app.localhost", Target: "http://127.0.0.1:44002", LeaseExpiresAt: now.Add(-time.Second), CWD: "/old"}
	if err := store.Save([]Route{expired}); err != nil {
		t.Fatal(err)
	}
	result := reserveTestRun(t, store, "new", "app.localhost", "http://127.0.0.1:44002", "/new", now)
	if result.Routes[0].Route.Host != "app.localhost" || result.Routes[0].Route.PendingTarget != expired.Target {
		t.Fatalf("reservation did not reclaim expired active route: %#v", result)
	}
}

func TestActivateRoutesPromotesWholeRunWithoutChangingIdentity(t *testing.T) {
	store := NewMemoryStore()
	now := time.Now().UTC()
	result, err := ReserveRoutes(store, ReservationRequest{RunID: "run-a", TTL: time.Minute, Routes: []RouteReservation{
		{DesiredHost: "web.localhost", Target: "http://127.0.0.1:45001", CWD: "/work/web"},
		{DesiredHost: "api.localhost", Target: "http://127.0.0.1:45002", CWD: "/work/api"},
	}}, now)
	if err != nil {
		t.Fatal(err)
	}
	refs := reservationRefs(result)
	activated, err := ActivateRoutes(store, "run-a", refs, now.Add(time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(activated) != 2 {
		t.Fatalf("activated = %#v", activated)
	}
	for i, route := range activated {
		if route.EffectiveState() != RouteStateActive || route.Target == "" || route.PendingTarget != "" || route.ID != refs[i].ID || route.Generation != refs[i].Generation {
			t.Fatalf("activated route = %#v, ref = %#v", route, refs[i])
		}
	}
}

func TestActivateAndReleaseRequireCompleteRun(t *testing.T) {
	store := NewMemoryStore()
	now := time.Now().UTC()
	result, err := ReserveRoutes(store, ReservationRequest{RunID: "run-a", TTL: time.Minute, Routes: []RouteReservation{
		{DesiredHost: "web.localhost", Target: "http://127.0.0.1:45501", CWD: "/work/web"},
		{DesiredHost: "api.localhost", Target: "http://127.0.0.1:45502", CWD: "/work/api"},
	}}, now)
	if err != nil {
		t.Fatal(err)
	}
	one := result.PendingRefs()[:1]
	if _, err := ActivateRoutes(store, "run-a", one, now, time.Minute); !errors.Is(err, ErrRouteRefMismatch) {
		t.Fatalf("partial activation error = %v", err)
	}
	if err := ReleaseRoutes(store, "run-a", one); !errors.Is(err, ErrRouteRefMismatch) {
		t.Fatalf("partial release error = %v", err)
	}
	routes, _ := store.Load()
	if len(routes) != 2 || routes[0].EffectiveState() != RouteStatePending || routes[1].EffectiveState() != RouteStatePending {
		t.Fatalf("routes mutated by partial operation: %#v", routes)
	}
}

func TestActivateRoutesMismatchIsAtomic(t *testing.T) {
	store := NewMemoryStore()
	now := time.Now().UTC()
	result := reserveTestRun(t, store, "run-a", "app.localhost", "http://127.0.0.1:46001", "/work/app", now)
	bad := result.Routes[0].Route.Ref()
	bad.Generation++
	_, err := ActivateRoutes(store, "run-a", []RouteRef{bad}, now, time.Minute)
	if !errors.Is(err, ErrRouteRefMismatch) {
		t.Fatalf("error = %v", err)
	}
	routes, _ := store.Load()
	if routes[0].EffectiveState() != RouteStatePending {
		t.Fatalf("route mutated after failed activation: %#v", routes[0])
	}
}

func TestRenewRoutesAllowsSurvivingSubsetAfterExplicitDelete(t *testing.T) {
	now := time.Now().UTC()
	store := NewMemoryStore()
	result, err := ReserveRoutes(store, ReservationRequest{RunID: "group", TTL: time.Minute, Routes: []RouteReservation{
		{DesiredHost: "web.localhost", Target: "http://127.0.0.1:42001", CWD: "/web"},
		{DesiredHost: "api.localhost", Target: "http://127.0.0.1:42002", CWD: "/api"},
	}}, now)
	if err != nil {
		t.Fatal(err)
	}
	refs := result.PendingRefs()
	if _, err := ActivateRoutes(store, "group", refs, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := DeleteRouteRef(store, refs[0]); err != nil {
		t.Fatal(err)
	}
	if err := RenewRoutes(store, "group", []RouteRef{refs[1]}, now.Add(30*time.Second), time.Minute); err != nil {
		t.Fatal(err)
	}
	routes, _ := store.Load()
	if len(routes) != 1 || routes[0].Ref() != refs[1] || !routes[0].LeaseExpiresAt.After(now.Add(time.Minute)) {
		t.Fatalf("surviving renewal = %#v", routes)
	}
}

func TestReleaseRenewAndDeleteUseIdentity(t *testing.T) {
	store := NewMemoryStore()
	now := time.Now().UTC()
	result := reserveTestRun(t, store, "run-a", "app.localhost", "http://127.0.0.1:47001", "/work/app", now)
	ref := result.Routes[0].Route.Ref()
	if err := RenewRoutes(store, "run-a", []RouteRef{ref}, now.Add(time.Second), 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	routes, _ := store.Load()
	if !routes[0].ReservationExpiresAt.Equal(now.Add(time.Second).Add(2 * time.Minute)) {
		t.Fatalf("renewed expiry = %v", routes[0].ReservationExpiresAt)
	}
	wrong := RouteRef{ID: ref.ID, Generation: ref.Generation + 1}
	if err := DeleteRouteRef(store, wrong); !errors.Is(err, ErrRouteRefMismatch) {
		t.Fatalf("delete error = %v", err)
	}
	if err := ReleaseRoutes(store, "run-a", []RouteRef{ref}); err != nil {
		t.Fatal(err)
	}
	routes, _ = store.Load()
	if len(routes) != 0 {
		t.Fatalf("routes after release = %#v", routes)
	}
}

func reserveTestRun(t *testing.T, store Store, runID, host, target, cwd string, now time.Time) ReservationResult {
	t.Helper()
	result, err := ReserveRoutes(store, ReservationRequest{RunID: runID, TTL: time.Minute, Routes: []RouteReservation{{DesiredHost: host, Target: target, CWD: cwd}}}, now)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func reservationRefs(result ReservationResult) []RouteRef {
	refs := make([]RouteRef, len(result.Routes))
	for i := range result.Routes {
		refs[i] = result.Routes[i].Route.Ref()
	}
	return refs
}
