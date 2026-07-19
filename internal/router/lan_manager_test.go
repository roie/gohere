package router

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/roie/gohere/internal/laninterface"
	"github.com/roie/gohere/internal/lanmdns"
)

func TestLANTrustTokenIsCompactAndRetains128BitsOfEntropy(t *testing.T) {
	token, err := newLANTrustToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != 22 {
		t.Fatalf("token length = %d, want 22: %q", len(token), token)
	}
	for _, char := range token {
		if !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') && !(char >= '0' && char <= '9') && char != '-' && char != '_' {
			t.Fatalf("token contains non-URL-safe character %q", char)
		}
	}
}

func TestLANManagerCoordinatesRouteBeforeMDNSActivationAndRemovesIt(t *testing.T) {
	store, server, route := lanManagerFixture(t)
	fakeIngress := &fakeLANIngress{}
	var responder *fakeLANHostnameResponder
	manager := newLANManager(t.Context(), server, lanManagerConfig{
		store: store,
		selectInterface: func(context.Context) (laninterface.Candidate, error) {
			return laninterface.Candidate{Index: 7, Name: "Wi-Fi", Prefix: netip.MustParsePrefix("192.168.1.42/24"), Flags: net.FlagUp | net.FlagMulticast}, nil
		},
		issueCertificate: func(string, time.Time) (tls.Certificate, error) { return tls.Certificate{}, nil },
		prepareTrust: func(laninterface.Candidate, string) (lanTrustRegistration, error) {
			return lanTrustRegistration{token: "token", setupURL: "http://192.168.1.42/__gohere/trust/token", fingerprint: "AA:BB"}, nil
		},
		startIngress: func(context.Context, *Server, string, string) (lanIngress, error) { return fakeIngress, nil },
		newResponder: func(_ context.Context, _ lanmdns.Interface, coordinator lanmdns.Coordinator) (lanHostnameResponder, error) {
			responder = &fakeLANHostnameResponder{coordinator: coordinator}
			return responder, nil
		},
		now: func() time.Time { return time.Date(2026, 7, 18, 20, 0, 0, 0, time.UTC) },
	})

	result, err := manager.Create(t.Context(), route.Ref())
	if err != nil {
		t.Fatal(err)
	}
	if result.Hostname != "shop.local." || result.URL != "https://shop.local" || result.SetupURL != "http://192.168.1.42/__gohere/trust/token" || result.Fingerprint != "AA:BB" {
		t.Fatalf("result = %#v", result)
	}
	stored, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	share := stored[0].LANShare
	if share == nil || share.State != LANShareActive || share.Hostname != "shop.local." || share.InterfaceIndex != 7 {
		t.Fatalf("share = %#v", share)
	}
	if responder == nil || responder.registered != "shop.local." {
		t.Fatalf("responder = %#v", responder)
	}

	if err := manager.Remove(t.Context(), route.Ref()); err != nil {
		t.Fatal(err)
	}
	stored, _ = store.Load()
	if stored[0].LANShare != nil || !responder.registration.closed {
		t.Fatalf("removal left share=%#v registration=%#v", stored[0].LANShare, responder.registration)
	}
	if !fakeIngress.closed {
		t.Fatal("last share did not close ingress")
	}
}

func TestLANManagerRotatesCertificateWithoutReplacingMDNSRegistration(t *testing.T) {
	store, server, route := lanManagerFixture(t)
	now := time.Date(2026, 7, 19, 1, 0, 0, 0, time.UTC)
	issueCalls := 0
	var responder *fakeLANHostnameResponder
	manager := newLANManager(t.Context(), server, lanManagerConfig{
		store: store,
		selectInterface: func(context.Context) (laninterface.Candidate, error) {
			return laninterface.Candidate{Index: 7, Name: "Wi-Fi", Prefix: netip.MustParsePrefix("192.168.1.42/24"), Flags: net.FlagUp | net.FlagMulticast}, nil
		},
		issueCertificate: func(string, time.Time) (tls.Certificate, error) {
			issueCalls++
			notAfter := now.Add(30 * time.Minute)
			if issueCalls > 1 {
				notAfter = now.Add(24 * time.Hour)
			}
			return tls.Certificate{Leaf: &x509.Certificate{NotAfter: notAfter}}, nil
		},
		startIngress: func(context.Context, *Server, string, string) (lanIngress, error) { return &fakeLANIngress{}, nil },
		newResponder: func(_ context.Context, _ lanmdns.Interface, coordinator lanmdns.Coordinator) (lanHostnameResponder, error) {
			responder = &fakeLANHostnameResponder{coordinator: coordinator}
			return responder, nil
		},
		networkStillValid: func(context.Context, laninterface.Candidate) bool { return true },
		now:               func() time.Time { return now },
	})
	if _, err := manager.Create(t.Context(), route.Ref()); err != nil {
		t.Fatal(err)
	}
	manager.reconcileNetwork(t.Context())
	if issueCalls != 2 {
		t.Fatalf("certificate issue calls = %d, want 2", issueCalls)
	}
	if responder.registerCalls != 1 {
		t.Fatalf("mDNS register calls = %d, want 1", responder.registerCalls)
	}
	server.lanMu.RLock()
	rotated := server.lanRoutes["shop.local."].certificate.Leaf
	server.lanMu.RUnlock()
	if rotated == nil || !rotated.NotAfter.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("rotated certificate = %#v", rotated)
	}
}

func TestLANManagerRollsBackCoordinatorFailure(t *testing.T) {
	store, server, route := lanManagerFixture(t)
	manager := newLANManager(t.Context(), server, lanManagerConfig{
		store: store,
		selectInterface: func(context.Context) (laninterface.Candidate, error) {
			return laninterface.Candidate{Index: 7, Name: "Wi-Fi", Prefix: netip.MustParsePrefix("192.168.1.42/24"), Flags: net.FlagUp | net.FlagMulticast}, nil
		},
		issueCertificate: func(string, time.Time) (tls.Certificate, error) {
			return tls.Certificate{}, errors.New("certificate failed")
		},
		startIngress: func(context.Context, *Server, string, string) (lanIngress, error) { return &fakeLANIngress{}, nil },
		newResponder: func(_ context.Context, _ lanmdns.Interface, coordinator lanmdns.Coordinator) (lanHostnameResponder, error) {
			return &fakeLANHostnameResponder{coordinator: coordinator}, nil
		},
		now: time.Now,
	})
	if _, err := manager.Create(t.Context(), route.Ref()); err == nil {
		t.Fatal("Create() error = nil")
	}
	stored, _ := store.Load()
	if stored[0].LANShare != nil {
		t.Fatalf("failed creation left share = %#v", stored[0].LANShare)
	}
	if _, err := server.LANTLSConfig().GetCertificate(&tls.ClientHelloInfo{ServerName: "shop.local"}); err == nil {
		t.Fatal("failed creation left TLS registration")
	}
}

func TestLANManagerRecoversOnlyVerifiedReachableRoute(t *testing.T) {
	store, server, route := lanManagerFixture(t)
	route.LANShare = &LANShare{State: LANShareActive, RequestedHostname: "shop.local.", Hostname: "shop.local.", CreatedAt: time.Now()}
	if err := store.Save([]Route{route}); err != nil {
		t.Fatal(err)
	}
	var responder *fakeLANHostnameResponder
	manager := newTestLANManager(t, store, server, func(value *fakeLANHostnameResponder) { responder = value })
	manager.config.routeOwnerVerified = func(Route) bool { return true }
	manager.config.targetReachable = func(Route) bool { return true }
	if err := manager.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	stored, _ := store.Load()
	if responder == nil || stored[0].LANShare == nil || stored[0].LANShare.State != LANShareActive {
		t.Fatalf("recovery responder=%#v share=%#v", responder, stored[0].LANShare)
	}
}

func TestLANManagerRecoversRouteAfterAuthenticatedLeaseRenewal(t *testing.T) {
	store, server, route := lanManagerFixture(t)
	route.LANShare = &LANShare{State: LANShareSuspended, RequestedHostname: "shop.local.", Hostname: "shop.local.", CreatedAt: time.Now()}
	if err := store.Save([]Route{route}); err != nil {
		t.Fatal(err)
	}
	var responder *fakeLANHostnameResponder
	manager := newTestLANManager(t, store, server, func(value *fakeLANHostnameResponder) { responder = value })
	manager.config.targetReachable = func(Route) bool { return true }
	if err := manager.RecoverVerified(t.Context(), route.Ref()); err != nil {
		t.Fatal(err)
	}
	if responder == nil {
		t.Fatal("authenticated renewal did not recover LAN share")
	}
}

func TestLANManagerSuspendsUnverifiedRouteWithoutExposure(t *testing.T) {
	store, server, route := lanManagerFixture(t)
	route.LANShare = &LANShare{State: LANShareActive, RequestedHostname: "shop.local.", Hostname: "shop.local.", CreatedAt: time.Now()}
	if err := store.Save([]Route{route}); err != nil {
		t.Fatal(err)
	}
	manager := newTestLANManager(t, store, server, func(*fakeLANHostnameResponder) { t.Fatal("unverified route started responder") })
	manager.config.routeOwnerVerified = func(Route) bool { return false }
	manager.config.targetReachable = func(Route) bool { return true }
	if err := manager.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	stored, _ := store.Load()
	if stored[0].LANShare == nil || stored[0].LANShare.State != LANShareSuspended || stored[0].LANShare.SuspendedReason == "" {
		t.Fatalf("suspended share = %#v", stored[0].LANShare)
	}
}

func TestSelectedLANNetworkRejectsPublicProfileTransition(t *testing.T) {
	selected := laninterface.Candidate{Index: 7, Name: "Wi-Fi", Prefix: netip.MustParsePrefix("192.168.1.42/24"), Flags: net.FlagUp | net.FlagMulticast, Profile: laninterface.ProfilePrivate}
	if !selectedLANNetworkStillValid(selected, []laninterface.Candidate{selected}) {
		t.Fatal("unchanged private network was rejected")
	}
	becamePublic := selected
	becamePublic.Profile = laninterface.ProfilePublic
	if selectedLANNetworkStillValid(selected, []laninterface.Candidate{becamePublic}) {
		t.Fatal("public profile transition remained valid")
	}
}

func TestLANManagerSuspendsAndWithdrawsWhenSelectedNetworkDisappears(t *testing.T) {
	store, server, route := lanManagerFixture(t)
	var responder *fakeLANHostnameResponder
	manager := newTestLANManager(t, store, server, func(value *fakeLANHostnameResponder) { responder = value })
	if _, err := manager.Create(t.Context(), route.Ref()); err != nil {
		t.Fatal(err)
	}
	manager.config.networkStillValid = func(context.Context, laninterface.Candidate) bool { return false }
	manager.config.selectInterface = func(context.Context) (laninterface.Candidate, error) {
		return laninterface.Candidate{}, laninterface.ErrNoPrivateNetwork
	}
	manager.reconcileNetwork(t.Context())
	stored, _ := store.Load()
	if responder == nil || !responder.registration.closed || stored[0].LANShare.State != LANShareSuspended {
		t.Fatalf("network loss responder=%#v share=%#v", responder, stored[0].LANShare)
	}
}

func TestLANShareAdminHandlerCreatesAndRemovesShare(t *testing.T) {
	store, server, route := lanManagerFixture(t)
	server.token = "secret"
	server.lanManager = newTestLANManager(t, store, server, func(*fakeLANHostnameResponder) {})
	payload, err := json.Marshal(route.Ref())
	if err != nil {
		t.Fatal(err)
	}
	create := httptest.NewRequest(http.MethodPost, "/v2/lan-shares", bytes.NewReader(payload))
	create.Header.Set("X-Gohere-Token", "secret")
	createResponse := httptest.NewRecorder()
	server.AdminHandler().ServeHTTP(createResponse, create)
	if createResponse.Code != http.StatusOK {
		t.Fatalf("create status = %d, body = %q", createResponse.Code, createResponse.Body.String())
	}
	var result LANShareResult
	if err := json.NewDecoder(createResponse.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Hostname != "shop.local." {
		t.Fatalf("result = %#v", result)
	}

	remove := httptest.NewRequest(http.MethodDelete, "/v2/lan-shares", bytes.NewReader(payload))
	remove.Header.Set("X-Gohere-Token", "secret")
	removeResponse := httptest.NewRecorder()
	server.AdminHandler().ServeHTTP(removeResponse, remove)
	if removeResponse.Code != http.StatusNoContent {
		t.Fatalf("remove status = %d, body = %q", removeResponse.Code, removeResponse.Body.String())
	}
}

func TestDeletingRouteWithdrawsLANShareFirst(t *testing.T) {
	store, server, route := lanManagerFixture(t)
	server.token = "secret"
	var responder *fakeLANHostnameResponder
	server.lanManager = newTestLANManager(t, store, server, func(value *fakeLANHostnameResponder) { responder = value })
	if _, err := server.lanManager.Create(t.Context(), route.Ref()); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodDelete, "/v2/routes/route-1/1", nil)
	request.Header.Set("X-Gohere-Token", "secret")
	response := httptest.NewRecorder()
	server.AdminHandler().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
	if responder == nil || !responder.registration.closed {
		t.Fatal("route deletion did not withdraw mDNS registration")
	}
}

func TestLANManagerCoordinatesRuntimeRename(t *testing.T) {
	store, server, route := lanManagerFixture(t)
	var responder *fakeLANHostnameResponder
	manager := newTestLANManager(t, store, server, func(value *fakeLANHostnameResponder) { responder = value })
	if _, err := manager.Create(t.Context(), route.Ref()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Prepare(t.Context(), lanmdns.Change{Registration: "registration-1", Requested: "shop.local.", Previous: "shop.local.", Proposed: "shop-2.local."}); err != nil {
		t.Fatal(err)
	}
	stored, _ := store.Load()
	if stored[0].LANShare.Hostname != "shop-2.local." {
		t.Fatalf("hostname = %q", stored[0].LANShare.Hostname)
	}
	if _, err := server.LANTLSConfig().GetCertificate(&tls.ClientHelloInfo{ServerName: "shop.local"}); err == nil {
		t.Fatal("old hostname still has certificate")
	}
	if responder == nil {
		t.Fatal("responder was not created")
	}
}

func newTestLANManager(t *testing.T, store Store, server *Server, capture func(*fakeLANHostnameResponder)) *LANManager {
	t.Helper()
	return newLANManager(t.Context(), server, lanManagerConfig{
		store: store,
		selectInterface: func(context.Context) (laninterface.Candidate, error) {
			return laninterface.Candidate{Index: 7, Name: "Wi-Fi", Prefix: netip.MustParsePrefix("192.168.1.42/24"), Flags: net.FlagUp | net.FlagMulticast}, nil
		},
		issueCertificate: func(string, time.Time) (tls.Certificate, error) { return tls.Certificate{}, nil },
		startIngress:     func(context.Context, *Server, string, string) (lanIngress, error) { return &fakeLANIngress{}, nil },
		newResponder: func(_ context.Context, _ lanmdns.Interface, coordinator lanmdns.Coordinator) (lanHostnameResponder, error) {
			responder := &fakeLANHostnameResponder{coordinator: coordinator}
			capture(responder)
			return responder, nil
		},
		now: time.Now,
	})
}

func lanManagerFixture(t *testing.T) (Store, *Server, Route) {
	t.Helper()
	store := NewMemoryStore()
	route := Route{ID: "route-1", Generation: 1, State: RouteStateActive, Host: "shop.localhost", Target: "http://127.0.0.1:5173", PreferredScheme: "https"}
	if err := store.Save([]Route{route}); err != nil {
		t.Fatal(err)
	}
	return store, NewServer(Config{Store: store}), route
}

type fakeLANIngress struct{ closed bool }

func (i *fakeLANIngress) Close() error { i.closed = true; return nil }

type fakeLANHostnameResponder struct {
	coordinator   lanmdns.Coordinator
	registration  fakeLANRegistration
	registered    string
	registerCalls int
	closed        bool
}

func (r *fakeLANHostnameResponder) Register(ctx context.Context, hostname string) (lanmdns.Registration, error) {
	r.registered = hostname
	r.registerCalls++
	r.registration = fakeLANRegistration{id: "registration-1", requested: hostname, current: hostname}
	if err := r.coordinator.Prepare(ctx, lanmdns.Change{Registration: r.registration.id, Requested: hostname, Proposed: hostname}); err != nil {
		return nil, err
	}
	return &r.registration, nil
}
func (r *fakeLANHostnameResponder) Close() error { r.closed = true; return nil }

type fakeLANRegistration struct {
	id        lanmdns.RegistrationID
	requested string
	current   string
	closed    bool
}

func (r *fakeLANRegistration) ID() lanmdns.RegistrationID  { return r.id }
func (r *fakeLANRegistration) RequestedHostname() string   { return r.requested }
func (r *fakeLANRegistration) CurrentHostname() string     { return r.current }
func (r *fakeLANRegistration) Close(context.Context) error { r.closed = true; return nil }
