package wsledge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/roie/gohere/internal/router"
)

func TestProbeRouterIdentityRequiresExactInstance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != router.RouterIdentityPath || request.Host != "gohere-identity.localhost" {
			t.Fatalf("request = %s host %s", request.URL.Path, request.Host)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"routerInstanceId": "router-1"})
	}))
	defer server.Close()

	matched, observed, err := ProbeRouterIdentity(t.Context(), server.URL, "router-1")
	if err != nil || !matched || observed != "router-1" {
		t.Fatalf("matched = %v, observed = %q, err = %v", matched, observed, err)
	}
	matched, observed, err = ProbeRouterIdentity(t.Context(), server.URL, "router-2")
	if err != nil || matched || observed != "router-1" {
		t.Fatalf("matched = %v, observed = %q, err = %v", matched, observed, err)
	}
}

func TestProbeRouterIdentityRejectsPlausibleNonIdentityBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Write([]byte(`{"status":"gohere-router"}`))
	}))
	defer server.Close()

	matched, _, err := ProbeRouterIdentity(t.Context(), server.URL, "router-1")
	if err != nil {
		t.Fatal(err)
	}
	if matched {
		t.Fatal("generic health body was accepted as router identity")
	}
}
