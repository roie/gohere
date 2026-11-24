package router

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	localcert "github.com/roie/gohere/internal/cert"
	"github.com/roie/gohere/internal/laninterface"
	"github.com/roie/gohere/internal/lanmdns"
)

func TestRequestedLANShareMapsSingleAndWorkspaceHosts(t *testing.T) {
	now := time.Now()
	for routeHost, want := range map[string]string{
		"app.localhost":      "app.local.",
		"web.repo.localhost": "web.repo.local.",
		"api.repo.localhost": "api.repo.local.",
	} {
		share := requestedLANShare("lan", routeHost, now)
		if share == nil || share.RequestedHostname != want {
			t.Fatalf("requestedLANShare(%q) = %#v, want %s", routeHost, share, want)
		}
	}
	for _, routeHost := range []string{"localhost", "app.local", "app.example"} {
		if share := requestedLANShare("lan", routeHost, now); share != nil {
			t.Fatalf("requestedLANShare(%q) = %#v, want nil", routeHost, share)
		}
	}
}

func TestNormalizeLANHostnameAcceptsWorkspaceNames(t *testing.T) {
	for input, want := range map[string]string{
		"app.local":          "app.local.",
		"Web.Repo.Local:443": "web.repo.local.",
		"api.repo.local.":    "api.repo.local.",
		"web-2.repo.local":   "web-2.repo.local.",
	} {
		if got, err := normalizeLANHostname(input); err != nil || got != want {
			t.Fatalf("normalizeLANHostname(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", ".local", "web..local", "-web.repo.local", "web.-repo.local", "web_repo.local", "web.repo.localhost", "web.repo.example"} {
		if got, err := normalizeLANHostname(input); err == nil {
			t.Fatalf("normalizeLANHostname(%q) = %q, want error", input, got)
		}
	}
}

func TestLANManagerSharesWorkspaceChildrenOverHTTPS(t *testing.T) {
	upstreamHost := make(chan string, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHost <- r.Host
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	web := Route{ID: "web", Generation: 1, State: RouteStateActive, Host: "web.repo.localhost", Target: upstream.URL, PreferredScheme: "https"}
	api := Route{ID: "api", Generation: 1, State: RouteStateActive, Host: "api.repo.localhost", Target: upstream.URL, PreferredScheme: "https"}
	store := NewMemoryStore()
	if err := store.Save([]Route{web, api}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(Config{Store: store})
	certificates := localcert.Store{StateDir: t.TempDir()}
	ca, err := certificates.EnsureCA()
	if err != nil {
		t.Fatal(err)
	}
	manager := newLANManager(t.Context(), server, lanManagerConfig{
		store: store,
		selectInterface: func(context.Context) (laninterface.Candidate, error) {
			return laninterface.Candidate{Index: 7, Name: "Wi-Fi", Prefix: netip.MustParsePrefix("192.168.1.42/24"), Flags: net.FlagUp | net.FlagMulticast}, nil
		},
		issueCertificate: func(host string, now time.Time) (lanCertificateMaterial, error) {
			certificate, err := certificates.IssueLANHostCert(ca, host, now)
			return lanCertificateMaterial{certificate: certificate}, err
		},
		startIngress: func(context.Context, *Server, string, string) (lanIngress, error) { return &fakeLANIngress{}, nil },
		newResponder: func(_ context.Context, _ lanmdns.Interface, coordinator lanmdns.Coordinator) (lanHostnameResponder, error) {
			return &workspaceResponder{coordinator: coordinator}, nil
		},
		now: time.Now,
	})

	for _, test := range []struct {
		route    Route
		hostname string
	}{{web, "web.repo.local"}, {api, "api.repo.local"}} {
		result, err := manager.Create(t.Context(), test.route.Ref())
		if err != nil {
			t.Fatalf("Create(%s): %v", test.route.Host, err)
		}
		if result.Hostname != test.hostname+"." || result.URL != "https://"+test.hostname {
			t.Fatalf("Create(%s) = %#v", test.route.Host, result)
		}
		assertLANCertificateFor(t, server, test.hostname)
		assertLANRequestReaches(t, server, test.hostname, test.route.Host, upstreamHost)
	}

	if err := manager.Prepare(t.Context(), lanmdns.Change{Registration: "web.repo.local.", Requested: "web.repo.local.", Previous: "web.repo.local.", Proposed: "web-2.repo.local."}); err != nil {
		t.Fatal(err)
	}
	if _, err := lanCertificateForServer(server, "web.repo.local"); err == nil {
		t.Fatal("renamed workspace child kept its old certificate")
	}
	assertLANCertificateFor(t, server, "web-2.repo.local")
	assertLANRequestReaches(t, server, "web-2.repo.local", web.Host, upstreamHost)
	assertLANRequestReaches(t, server, "api.repo.local", api.Host, upstreamHost)
}

func assertLANCertificateFor(t *testing.T, server *Server, hostname string) {
	t.Helper()
	certificate, err := lanCertificateForServer(server, hostname)
	if err != nil {
		t.Fatalf("certificate for %s: %v", hostname, err)
	}
	if certificate.Leaf == nil {
		t.Fatalf("certificate for %s has no leaf", hostname)
	}
	if err := certificate.Leaf.VerifyHostname(hostname); err != nil {
		t.Fatal(err)
	}
}

func assertLANRequestReaches(t *testing.T, server *Server, hostname, routeHost string, upstreamHost <-chan string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "https://"+hostname+"/", nil)
	request.Host = hostname
	request.TLS = &tls.ConnectionState{ServerName: hostname}
	response := httptest.NewRecorder()
	server.LANHandler().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("%s status = %d, body = %q", hostname, response.Code, response.Body.String())
	}
	if got := <-upstreamHost; got != routeHost {
		t.Fatalf("%s upstream Host = %q, want %q", hostname, got, routeHost)
	}
}

type workspaceResponder struct {
	coordinator lanmdns.Coordinator
}

func (r *workspaceResponder) Register(ctx context.Context, hostname string) (lanmdns.Registration, error) {
	registration := &fakeLANRegistration{id: lanmdns.RegistrationID(hostname), requested: hostname, current: hostname}
	if err := r.coordinator.Prepare(ctx, lanmdns.Change{Registration: registration.id, Requested: hostname, Proposed: hostname}); err != nil {
		return nil, err
	}
	return registration, nil
}

func (r *workspaceResponder) Close() error { return nil }
