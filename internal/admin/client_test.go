package admin

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/roie/gohere/internal/router"
)

func TestClientHealthAndRoutes(t *testing.T) {
	store := router.NewMemoryStore()
	srv := router.NewServer(router.Config{Token: "secret", Store: store})
	httpSrv := httptest.NewServer(srv.AdminHandler())
	defer httpSrv.Close()

	client := NewClient(httpSrv.URL, "secret")
	if err := client.Health(t.Context()); err != nil {
		t.Fatal(err)
	}

	route := router.Route{
		Host:            "eventca.localhost",
		Target:          "http://127.0.0.1:49231",
		PreferredScheme: "http",
		PID:             12345,
		CWD:             "/tmp/eventca",
		Name:            "eventca",
		StartedAt:       time.Date(2026, 5, 16, 0, 0, 0, 0, time.UTC),
	}
	if err := client.UpsertRoute(t.Context(), route); err != nil {
		t.Fatal(err)
	}
	routes, err := client.Routes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0].Host != route.Host {
		t.Fatalf("routes = %#v", routes)
	}
	if err := client.DeleteRoute(t.Context(), route.Host); err != nil {
		t.Fatal(err)
	}
	routes, err = client.Routes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 0 {
		t.Fatalf("routes after delete = %#v", routes)
	}
}

func TestClientReservationLifecycle(t *testing.T) {
	store := router.NewMemoryStore()
	srv := router.NewServer(router.Config{Token: "secret", Store: store})
	httpSrv := httptest.NewServer(srv.AdminHandler())
	defer httpSrv.Close()
	client := NewClient(httpSrv.URL, "secret")

	result, err := client.ReserveRoutes(t.Context(), router.ReservationRequest{RunID: "run-a", Routes: []router.RouteReservation{{DesiredHost: "web.localhost", PreferredScheme: "http", Target: "http://127.0.0.1:48301", CWD: "/work/web"}}})
	if err != nil {
		t.Fatal(err)
	}
	refs := result.PendingRefs()
	active, err := client.ActivateRoutes(t.Context(), "run-a", refs)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].Target != "http://127.0.0.1:48301" {
		t.Fatalf("active = %#v", active)
	}
	if err := client.RenewRoutes(t.Context(), "run-a", refs); err != nil {
		t.Fatal(err)
	}
	if err := client.ReleaseRoutes(t.Context(), "run-a", refs); err != nil {
		t.Fatal(err)
	}
	routes, err := client.Routes(t.Context())
	if err != nil || len(routes) != 0 {
		t.Fatalf("routes/error = %#v/%v", routes, err)
	}
}

func TestClientReserveRoutesReportsSchemeConflict(t *testing.T) {
	store := router.NewMemoryStore()
	existing := router.Route{
		ID: "existing", Generation: 1, State: router.RouteStateActive,
		Host: "app.localhost", PreferredScheme: "https",
		Target: "http://127.0.0.1:42701", CWD: "/work/app",
		ProjectRoot: "/work", ProjectName: "work",
	}
	if err := store.Save([]router.Route{existing}); err != nil {
		t.Fatal(err)
	}
	srv := router.NewServer(router.Config{Token: "secret", Store: store})
	httpSrv := httptest.NewServer(srv.AdminHandler())
	defer httpSrv.Close()
	client := NewClient(httpSrv.URL, "secret")
	_, err := client.ReserveRoutes(t.Context(), router.ReservationRequest{
		RunID: "new-run",
		Routes: []router.RouteReservation{{
			DesiredHost: "app.localhost", PreferredScheme: "http",
			Target: "http://127.0.0.1:42702", CWD: "/work/app",
			ProjectRoot: "/work", ProjectName: "work",
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "409 Conflict") ||
		!strings.Contains(err.Error(), "already uses scheme https; stop it before requesting http") {
		t.Fatalf("error = %v", err)
	}
	routes, loadErr := store.Load()
	if loadErr != nil || len(routes) != 1 || routes[0].ID != existing.ID {
		t.Fatalf("routes/error = %#v/%v", routes, loadErr)
	}
}

func TestClientCreatesAndDeletesLANShare(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests++
		if request.URL.Path != "/v2/lan-shares" || request.Header.Get("X-Gohere-Token") != "secret" {
			t.Fatalf("request = %s %s, token %q", request.Method, request.URL.Path, request.Header.Get("X-Gohere-Token"))
		}
		if request.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"hostname":"shop.local.","url":"https://shop.local","address":"192.168.1.42"}`))
			return
		}
		if request.Method != http.MethodDelete {
			t.Fatalf("method = %s", request.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client := NewClient(server.URL, "secret")
	ref := router.RouteRef{ID: "route-1", Generation: 1}
	result, err := client.CreateLANShare(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if result.Hostname != "shop.local." || result.URL != "https://shop.local" {
		t.Fatalf("result = %#v", result)
	}
	if err := client.DeleteLANShare(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d", requests)
	}
}

func TestClientDeleteRouteRef(t *testing.T) {
	store := router.NewMemoryStore()
	route := router.Route{ID: "route-1", Generation: 2, State: router.RouteStateActive, Host: "web.localhost", Target: "http://127.0.0.1:48302"}
	if err := store.Save([]router.Route{route}); err != nil {
		t.Fatal(err)
	}
	srv := router.NewServer(router.Config{Token: "secret", Store: store})
	httpSrv := httptest.NewServer(srv.AdminHandler())
	defer httpSrv.Close()
	client := NewClient(httpSrv.URL, "secret")
	if err := client.DeleteRouteRef(t.Context(), route.Ref()); err != nil {
		t.Fatal(err)
	}
}

func TestClientRejectsLegacyHealthResponse(t *testing.T) {
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok\n"))
	}))
	defer httpSrv.Close()

	if err := NewClient(httpSrv.URL, "secret").Health(t.Context()); err == nil {
		t.Fatal("expected health body error")
	}
}

func TestClientRejectsUnhealthyRouter(t *testing.T) {
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusServiceUnavailable)
	}))
	defer httpSrv.Close()

	if err := NewClient(httpSrv.URL, "secret").Health(t.Context()); err == nil {
		t.Fatal("expected health error")
	}
}

func TestClientRejectsNonGohereHealthResponse(t *testing.T) {
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("not gohere\n"))
	}))
	defer httpSrv.Close()

	if err := NewClient(httpSrv.URL, "secret").Health(t.Context()); err == nil {
		t.Fatal("expected health body error")
	}
}

func TestClientReportsUnauthorizedRoutes(t *testing.T) {
	srv := router.NewServer(router.Config{Token: "server-token", Store: router.NewMemoryStore()})
	httpSrv := httptest.NewServer(srv.AdminHandler())
	defer httpSrv.Close()

	_, err := NewClient(httpSrv.URL, "client-token").Routes(t.Context())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Routes error = %v, want ErrUnauthorized", err)
	}
}

func TestClientRouteStatuses(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer target.Close()
	store := router.NewMemoryStore()
	if err := store.Save([]router.Route{{
		Host:   "web.localhost",
		Target: target.URL,
	}}); err != nil {
		t.Fatal(err)
	}
	srv := router.NewServer(router.Config{Token: "secret", Store: store})
	httpSrv := httptest.NewServer(srv.AdminHandler())
	defer httpSrv.Close()

	statuses, err := NewClient(httpSrv.URL, "secret").RouteStatuses(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Route.Host != "web.localhost" || statuses[0].Status != "ready" {
		t.Fatalf("statuses = %#v", statuses)
	}
}

func TestClientRouteStatusesReportsUnauthorized(t *testing.T) {
	srv := router.NewServer(router.Config{Token: "server-token", Store: router.NewMemoryStore()})
	httpSrv := httptest.NewServer(srv.AdminHandler())
	defer httpSrv.Close()

	_, err := NewClient(httpSrv.URL, "client-token").RouteStatuses(t.Context())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("RouteStatuses error = %v, want ErrUnauthorized", err)
	}
}

func TestClientRouteStatusesReportsUnsupported(t *testing.T) {
	httpSrv := httptest.NewServer(http.NotFoundHandler())
	defer httpSrv.Close()

	_, err := NewClient(httpSrv.URL, "secret").RouteStatuses(t.Context())
	if !errors.Is(err, ErrRouteStatusesUnsupported) {
		t.Fatalf("RouteStatuses error = %v, want ErrRouteStatusesUnsupported", err)
	}
}

func TestClientRouteStatusesIncludesErrorBody(t *testing.T) {
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "route status broken", http.StatusInternalServerError)
	}))
	defer httpSrv.Close()

	_, err := NewClient(httpSrv.URL, "secret").RouteStatuses(t.Context())
	if err == nil || !strings.Contains(err.Error(), "route status broken") {
		t.Fatalf("RouteStatuses error = %v, want response body", err)
	}
}

func TestClientDeleteRouteEscapesHostPathSegment(t *testing.T) {
	var gotPath string
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer httpSrv.Close()

	if err := NewClient(httpSrv.URL, "secret").DeleteRoute(t.Context(), "weird/host?x#y"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/routes/weird%2Fhost%3Fx%23y" {
		t.Fatalf("path = %q", gotPath)
	}
}

func TestClientShutdown(t *testing.T) {
	called := make(chan struct{}, 1)
	srv := router.NewServer(router.Config{Token: "secret", Store: router.NewMemoryStore(), Shutdown: func() { called <- struct{}{} }})
	httpSrv := httptest.NewServer(srv.AdminHandler())
	defer httpSrv.Close()

	if err := NewClient(httpSrv.URL, "secret").Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("shutdown handler was not called")
	}
}

func TestClientShutdownReportsUnauthorized(t *testing.T) {
	srv := router.NewServer(router.Config{Token: "server-token", Store: router.NewMemoryStore()})
	httpSrv := httptest.NewServer(srv.AdminHandler())
	defer httpSrv.Close()

	err := NewClient(httpSrv.URL, "client-token").Shutdown(t.Context())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Shutdown error = %v, want ErrUnauthorized", err)
	}
}

func TestClientProbeTarget(t *testing.T) {
	srv := router.NewServer(router.Config{Token: "secret", Store: router.NewMemoryStore()})
	httpSrv := httptest.NewServer(srv.AdminHandler())
	defer httpSrv.Close()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer target.Close()

	reachable, err := NewClient(httpSrv.URL, "secret").ProbeTarget(t.Context(), target.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !reachable {
		t.Fatal("expected target to be reachable")
	}
}

func TestClientProbeTargetReportsUnauthorized(t *testing.T) {
	srv := router.NewServer(router.Config{Token: "server-token", Store: router.NewMemoryStore()})
	httpSrv := httptest.NewServer(srv.AdminHandler())
	defer httpSrv.Close()

	_, err := NewClient(httpSrv.URL, "client-token").ProbeTarget(t.Context(), "http://127.0.0.1:1")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("ProbeTarget error = %v, want ErrUnauthorized", err)
	}
}

func TestNewClientUsesBoundedTimeout(t *testing.T) {
	client := NewClient("http://127.0.0.1:39399", "secret")

	if client.http.Timeout <= 0 {
		t.Fatal("client timeout must be bounded")
	}
	if client.http.Timeout < 5*time.Second {
		t.Fatalf("client timeout = %s, want at least 5s for route status probes", client.http.Timeout)
	}
}
