package router

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestRequestLANShareIsEmbeddedAndIdempotent(t *testing.T) {
	created := time.Date(2026, 7, 18, 17, 0, 0, 0, time.UTC)
	route := Route{ID: "route-1", Generation: 3, State: RouteStateActive, Host: "shop.localhost", Target: "http://127.0.0.1:5173"}
	store := NewMemoryStore()
	if err := store.Save([]Route{route}); err != nil {
		t.Fatal(err)
	}
	first, err := RequestLANShare(store, route.Ref(), created)
	if err != nil {
		t.Fatal(err)
	}
	second, err := RequestLANShare(store, route.Ref(), created.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if first.LANShare == nil || second.LANShare == nil {
		t.Fatal("LAN share was not stored on route")
	}
	if first.LANShare.State != LANShareRequested || first.LANShare.RequestedHostname != "shop.local." {
		t.Fatalf("LAN share = %#v", first.LANShare)
	}
	if second.LANShare.CreatedAt != created {
		t.Fatalf("idempotent request changed creation time to %s", second.LANShare.CreatedAt)
	}
}

func TestLANShareStateTransitionsFailClosed(t *testing.T) {
	route := Route{ID: "route-1", Generation: 1, State: RouteStateActive, Host: "shop.localhost", Target: "http://127.0.0.1:5173"}
	store := NewMemoryStore()
	if err := store.Save([]Route{route}); err != nil {
		t.Fatal(err)
	}
	if _, err := RequestLANShare(store, route.Ref(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := SetLANShareState(store, route.Ref(), LANShareActive); err != nil {
		t.Fatal(err)
	}
	if err := SetLANShareState(store, route.Ref(), LANShareRequested); err == nil {
		t.Fatal("active share returned to requested state")
	}
	if err := SetLANShareState(store, RouteRef{ID: route.ID, Generation: 2}, LANShareRemoving); !errors.Is(err, ErrRouteRefMismatch) {
		t.Fatalf("stale ref error = %v", err)
	}
}

func TestActivateLANShareStoresResolvedHostnameAndInterface(t *testing.T) {
	route := Route{ID: "route-1", Generation: 1, State: RouteStateActive, Host: "shop.localhost", Target: "http://127.0.0.1:5173"}
	store := NewMemoryStore()
	if err := store.Save([]Route{route}); err != nil {
		t.Fatal(err)
	}
	if _, err := RequestLANShare(store, route.Ref(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := ActivateLANShare(store, route.Ref(), LANActivation{
		Hostname: "shop-2.local.", InterfaceIndex: 7, InterfaceName: "Wi-Fi", Address: "192.168.1.42", Prefix: "192.168.1.42/24",
	}); err != nil {
		t.Fatal(err)
	}
	routes, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	share := routes[0].LANShare
	if share == nil || share.State != LANShareActive || share.Hostname != "shop-2.local." || share.InterfaceIndex != 7 || share.Address != "192.168.1.42" {
		t.Fatalf("LAN share = %#v", share)
	}
}

func TestRemoveLANSharePreservesRoute(t *testing.T) {
	route := Route{ID: "route-1", Generation: 1, State: RouteStateActive, Host: "shop.localhost", Target: "http://127.0.0.1:5173"}
	store := NewMemoryStore()
	if err := store.Save([]Route{route}); err != nil {
		t.Fatal(err)
	}
	if _, err := RequestLANShare(store, route.Ref(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := RemoveLANShare(store, route.Ref()); err != nil {
		t.Fatal(err)
	}
	routes, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0].LANShare != nil || routes[0].Host != route.Host {
		t.Fatalf("routes = %#v", routes)
	}
}

func TestReserveRoutesCreatesLANShareIntentAndReleaseRemovesIt(t *testing.T) {
	now := time.Date(2026, 7, 18, 18, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	result, err := ReserveRoutes(store, ReservationRequest{
		RunID: "run-1",
		TTL:   time.Minute,
		Routes: []RouteReservation{{
			DesiredHost:     "shop.localhost",
			PreferredScheme: "https",
			Target:          "http://127.0.0.1:5173",
			CWD:             "/project",
			ShareMode:       "lan",
		}},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	share := result.Routes[0].Route.LANShare
	if share == nil || share.State != LANShareRequested || share.RequestedHostname != "shop.local." || share.CreatedAt != now {
		t.Fatalf("LAN share = %#v", share)
	}
	refs := result.PendingRefs()
	if err := ReleaseRoutes(store, result.RunID, refs); err != nil {
		t.Fatal(err)
	}
	routes, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 0 {
		t.Fatalf("released routes = %#v", routes)
	}
}

func TestReserveRoutesAddsLANIntentWhenReusingRoute(t *testing.T) {
	now := time.Date(2026, 7, 18, 18, 30, 0, 0, time.UTC)
	store := NewMemoryStore()
	initial, err := ReserveRoutes(store, ReservationRequest{RunID: "run-1", TTL: time.Minute, Routes: []RouteReservation{{
		DesiredHost: "shop.localhost", PreferredScheme: "https", Target: "http://127.0.0.1:5173", CWD: "/project", OwnerCWD: "/project",
	}}}, now)
	if err != nil {
		t.Fatal(err)
	}
	refs := initial.PendingRefs()
	active, err := ActivateRoutes(store, "run-1", refs, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	activeRef := active[0].Ref()
	reused, err := ReserveRoutes(store, ReservationRequest{RunID: "run-2", TTL: time.Minute, Routes: []RouteReservation{{
		DesiredHost: "shop.localhost", PreferredScheme: "https", Target: "http://127.0.0.1:5173", CWD: "/project", OwnerCWD: "/project",
		Reuse: &activeRef, ShareMode: "lan",
	}}}, now.Add(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || len(reused.Routes) != 1 || !reused.Routes[0].Reused || reused.Routes[0].Route.LANShare == nil {
		t.Fatalf("reused result = %#v", reused)
	}
	stored, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].LANShare == nil {
		t.Fatalf("stored routes = %#v", stored)
	}
}

func TestLANShareRoundTripsWithRouteState(t *testing.T) {
	route := Route{Host: "shop.localhost", LANShare: &LANShare{State: LANShareSuspended, RequestedHostname: "shop.local.", Hostname: "shop-2.local.", InterfaceIndex: 7, Address: "192.168.1.42", Prefix: "192.168.1.42/24"}}
	payload, err := json.Marshal(route)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Route
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.LANShare == nil || *decoded.LANShare != *route.LANShare {
		t.Fatalf("decoded LAN share = %#v", decoded.LANShare)
	}
}
