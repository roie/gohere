package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/roie/gohere/internal/router"
)

func TestActivationRejectsOlderRouter(t *testing.T) {
	requests := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.URL.Path
		if r.URL.Path == "/v2/route-reservations/run/activate" {
			w.Write([]byte("[]"))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	refs := []router.RouteRef{{ID: "route", Generation: 1}}
	_, err := NewClient(server.URL, "token").ActivateRoutes(t.Context(), "run", refs, []router.RouteProcess{{Ref: refs[0], PID: 123, ProcessIdentity: "root"}})
	if err == nil {
		t.Fatal("older router accepted activation")
	}
	if got := <-requests; got != "/v3/route-reservations/run/activate" {
		t.Fatalf("activation path = %s", got)
	}
	select {
	case got := <-requests:
		t.Fatalf("unexpected fallback request: %s", got)
	default:
	}
}
