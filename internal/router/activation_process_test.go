package router

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestActivateRoutesPersistsProcessesByRef(t *testing.T) {
	store := NewRouteStore(filepath.Join(t.TempDir(), "routes.json"))
	now := time.Now().UTC()
	result, err := ReserveRoutes(store, ReservationRequest{RunID: "group", TTL: time.Minute, Routes: []RouteReservation{
		{DesiredHost: "web.localhost", PreferredScheme: "http", Target: "http://127.0.0.1:45001", CWD: "/web"},
		{DesiredHost: "api.localhost", PreferredScheme: "http", Target: "http://127.0.0.1:45002", CWD: "/api"},
	}}, now)
	if err != nil {
		t.Fatal(err)
	}
	refs := result.PendingRefs()
	processes := []RouteProcess{{Ref: refs[1], PID: 202, ProcessIdentity: "second"}, {Ref: refs[0], PID: 101, ProcessIdentity: "first"}}
	if _, err := ActivateRoutes(store, "group", refs, processes, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	routes, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 2 {
		t.Fatalf("routes = %+v", routes)
	}
	for _, route := range routes {
		if route.EffectiveState() != RouteStateActive {
			t.Fatalf("not active: %+v", route)
		}
		for _, process := range processes {
			if route.Ref() == process.Ref && (route.PID != process.PID || route.ProcessIdentity != process.ProcessIdentity) {
				t.Fatalf("process lost after reload: %+v, want %+v", route, process)
			}
		}
	}
}

func TestActivateRoutesRejectsInvalidProcessesAtomically(t *testing.T) {
	for _, kind := range []string{"stale-generation", "unknown-route", "duplicate", "zero-pid", "negative-pid", "blank-identity", "reused-route"} {
		t.Run(kind, func(t *testing.T) {
			store := NewMemoryStore()
			now := time.Now().UTC()
			existing := Route{ID: "external", Generation: 3, State: RouteStateActive, Host: "external.localhost", Target: "http://127.0.0.1:45003", PID: 303, ProcessIdentity: "external"}
			if err := store.Save([]Route{existing}); err != nil {
				t.Fatal(err)
			}
			result, err := ReserveRoutes(store, ReservationRequest{RunID: "group", TTL: time.Minute, Routes: []RouteReservation{
				{DesiredHost: "web.localhost", PreferredScheme: "http", Target: "http://127.0.0.1:45001", CWD: "/web"},
				{DesiredHost: "api.localhost", PreferredScheme: "http", Target: "http://127.0.0.1:45002", CWD: "/api"},
			}}, now)
			if err != nil {
				t.Fatal(err)
			}
			refs := result.PendingRefs()
			processes := []RouteProcess{{Ref: refs[0], PID: 101, ProcessIdentity: "first"}, {Ref: refs[1], PID: 202, ProcessIdentity: "second"}}
			switch kind {
			case "stale-generation":
				processes[1].Ref.Generation++
			case "unknown-route":
				processes[1].Ref.ID = "missing"
			case "duplicate":
				processes[1].Ref = refs[0]
			case "zero-pid":
				processes[1].PID = 0
			case "negative-pid":
				processes[1].PID = -1
			case "blank-identity":
				processes[1].ProcessIdentity = " "
			case "reused-route":
				processes[1].Ref = existing.Ref()
			}
			before, _ := store.Load()
			if _, err := ActivateRoutes(store, "group", refs, processes, now, time.Minute); err == nil {
				t.Fatal("invalid activation succeeded")
			}
			after, _ := store.Load()
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("failed activation changed routes: %+v", after)
			}
		})
	}
}
