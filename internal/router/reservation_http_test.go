package router

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReservationLifecycleAPI(t *testing.T) {
	store := NewMemoryStore()
	srv := NewServer(Config{Token: "secret", Store: store})
	request := ReservationRequest{RunID: "run-a", Routes: []RouteReservation{{DesiredHost: "web.localhost", PreferredScheme: "http", Target: "http://127.0.0.1:48001", CWD: "/work/web"}}}
	var body bytes.Buffer
	json.NewEncoder(&body).Encode(request)
	rec := reservationAPIRequest(t, srv, http.MethodPost, "/v2/route-reservations", &body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("reserve status/body = %d/%q", rec.Code, rec.Body.String())
	}
	var result ReservationResult
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if len(result.Routes) != 1 || result.Routes[0].Route.EffectiveState() != RouteStatePending || result.Routes[0].Route.ReservationExpiresAt.IsZero() {
		t.Fatalf("reservation = %#v", result)
	}

	refs := RouteRefsRequest{Refs: result.PendingRefs()}
	body.Reset()
	json.NewEncoder(&body).Encode(refs)
	rec = reservationAPIRequest(t, srv, http.MethodPost, "/v2/route-reservations/run-a/activate", &body)
	if rec.Code != http.StatusOK {
		t.Fatalf("activate status/body = %d/%q", rec.Code, rec.Body.String())
	}
	var active []Route
	if err := json.NewDecoder(rec.Body).Decode(&active); err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].EffectiveState() != RouteStateActive || active[0].Target != "http://127.0.0.1:48001" || active[0].LeaseExpiresAt.IsZero() {
		t.Fatalf("active routes = %#v", active)
	}

	oldExpiry := active[0].LeaseExpiresAt
	body.Reset()
	json.NewEncoder(&body).Encode(refs)
	rec = reservationAPIRequest(t, srv, http.MethodPost, "/v2/route-reservations/run-a/renew", &body)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("renew status/body = %d/%q", rec.Code, rec.Body.String())
	}
	routes, _ := store.Load()
	if routes[0].LeaseExpiresAt.Before(oldExpiry) {
		t.Fatalf("renewed expiry regressed: %v, old = %v", routes[0].LeaseExpiresAt, oldExpiry)
	}

	body.Reset()
	json.NewEncoder(&body).Encode(refs)
	rec = reservationAPIRequest(t, srv, http.MethodDelete, "/v2/route-reservations/run-a", &body)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("release status/body = %d/%q", rec.Code, rec.Body.String())
	}
	routes, _ = store.Load()
	if len(routes) != 0 {
		t.Fatalf("routes after release = %#v", routes)
	}
}

func TestReservationLifecycleAPIRejectsUnauthorizedAndPartialMutation(t *testing.T) {
	store := NewMemoryStore()
	srv := NewServer(Config{Token: "secret", Store: store})
	for _, test := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/v2/route-reservations"},
		{http.MethodPost, "/v2/route-reservations/run-a/activate"},
		{http.MethodPost, "/v2/route-reservations/run-a/renew"},
		{http.MethodDelete, "/v2/route-reservations/run-a"},
		{http.MethodDelete, "/v2/routes/route-1/1"},
	} {
		req := httptest.NewRequest(test.method, test.path, bytes.NewReader([]byte(`{}`)))
		rec := httptest.NewRecorder()
		srv.AdminHandler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s unauthorized status = %d", test.method, test.path, rec.Code)
		}
	}

	now := time.Now().UTC()
	result, err := ReserveRoutes(store, ReservationRequest{RunID: "run-a", TTL: time.Minute, Routes: []RouteReservation{
		{DesiredHost: "web.localhost", PreferredScheme: "http", Target: "http://127.0.0.1:48101", CWD: "/work/web"},
		{DesiredHost: "api.localhost", PreferredScheme: "http", Target: "http://127.0.0.1:48102", CWD: "/work/api"},
	}}, now)
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	json.NewEncoder(&body).Encode(RouteRefsRequest{Refs: result.PendingRefs()[:1]})
	rec := reservationAPIRequest(t, srv, http.MethodPost, "/v2/route-reservations/run-a/activate", &body)
	if rec.Code != http.StatusConflict {
		t.Fatalf("partial activate status/body = %d/%q", rec.Code, rec.Body.String())
	}
	routes, _ := store.Load()
	if routes[0].EffectiveState() != RouteStatePending || routes[1].EffectiveState() != RouteStatePending {
		t.Fatalf("partial API mutation = %#v", routes)
	}
}

func TestReservationLifecycleAPIRejectsMalformedAndExpiredRequestsWithoutMutation(t *testing.T) {
	store := NewMemoryStore()
	srv := NewServer(Config{Token: "secret", Store: store})
	rec := reservationAPIRequest(t, srv, http.MethodPost, "/v2/route-reservations", bytes.NewBufferString(`{"runId":`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed reserve status = %d", rec.Code)
	}
	routes, _ := store.Load()
	if len(routes) != 0 {
		t.Fatalf("malformed reserve mutated store: %#v", routes)
	}

	now := time.Now().UTC()
	result, err := ReserveRoutes(store, ReservationRequest{RunID: "run-a", TTL: time.Second, Routes: []RouteReservation{{DesiredHost: "web.localhost", PreferredScheme: "http", Target: "http://127.0.0.1:48103", CWD: "/work/web"}}}, now.Add(-2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	json.NewEncoder(&body).Encode(RouteRefsRequest{Refs: result.PendingRefs()})
	rec = reservationAPIRequest(t, srv, http.MethodPost, "/v2/route-reservations/run-a/activate", &body)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expired activate status/body = %d/%q", rec.Code, rec.Body.String())
	}
	routes, _ = store.Load()
	if len(routes) != 1 || routes[0].EffectiveState() != RouteStatePending {
		t.Fatalf("expired activation mutated route: %#v", routes)
	}
}

func TestReservationAPIReportsSchemeConflict(t *testing.T) {
	store := NewMemoryStore()
	existing := Route{
		ID: "existing", Generation: 1, State: RouteStateActive,
		Host: "app.localhost", PreferredScheme: "https",
		Target: "http://127.0.0.1:42601", CWD: "/work/app",
		ProjectRoot: "/work", ProjectName: "work",
	}
	if err := store.Save([]Route{existing}); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(Config{Token: "secret", Store: store})
	body := &bytes.Buffer{}
	if err := json.NewEncoder(body).Encode(ReservationRequest{
		RunID: "new-run",
		Routes: []RouteReservation{{
			DesiredHost: "app.localhost", PreferredScheme: "http",
			Target: "http://127.0.0.1:42602", CWD: "/work/app",
			ProjectRoot: "/work", ProjectName: "work",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	rec := reservationAPIRequest(t, srv, http.MethodPost, "/v2/route-reservations", body)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status/body = %d/%q", rec.Code, rec.Body.String())
	}
	want := "route app.localhost already uses scheme https; stop it before requesting http"
	if !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q", rec.Header().Get("Content-Type"))
	}
	routes, err := store.Load()
	if err != nil || len(routes) != 1 || routes[0].ID != existing.ID {
		t.Fatalf("routes/error = %#v/%v", routes, err)
	}
}

func TestIdentityDeleteAPI(t *testing.T) {
	store := NewMemoryStore()
	route := Route{ID: "route-1", Generation: 2, State: RouteStateActive, Host: "web.localhost", Target: "http://127.0.0.1:48201"}
	if err := store.Save([]Route{route}); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(Config{Token: "secret", Store: store})
	rec := reservationAPIRequest(t, srv, http.MethodDelete, "/v2/routes/route-1/3", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("mismatch delete status = %d", rec.Code)
	}
	rec = reservationAPIRequest(t, srv, http.MethodDelete, "/v2/routes/route-1/2", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status/body = %d/%q", rec.Code, rec.Body.String())
	}
}

func reservationAPIRequest(t *testing.T, srv *Server, method, path string, body *bytes.Buffer) *httptest.ResponseRecorder {
	t.Helper()
	var requestBody *bytes.Reader
	if body == nil {
		requestBody = bytes.NewReader(nil)
	} else {
		requestBody = bytes.NewReader(body.Bytes())
	}
	req := httptest.NewRequest(method, path, requestBody)
	req.Header.Set("X-Gohere-Token", "secret")
	rec := httptest.NewRecorder()
	srv.AdminHandler().ServeHTTP(rec, req)
	return rec
}
