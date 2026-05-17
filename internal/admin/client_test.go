package admin

import (
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
