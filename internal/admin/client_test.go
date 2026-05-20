package admin

import (
	"errors"
	"net/http"
	"net/http/httptest"
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
		Host:      "eventca.localhost",
		Target:    "http://127.0.0.1:49231",
		PID:       12345,
		CWD:       "/tmp/eventca",
		Name:      "eventca",
		StartedAt: time.Date(2026, 5, 16, 0, 0, 0, 0, time.UTC),
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
	if client.http.Timeout > 2*time.Second {
		t.Fatalf("client timeout = %s, want at most 2s", client.http.Timeout)
	}
}
