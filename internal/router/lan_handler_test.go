package router

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLANHandlerRoutesOnlyRegisteredActiveShare(t *testing.T) {
	upstreamHost := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHost <- r.Host
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	route := Route{ID: "route-1", Generation: 1, State: RouteStateActive, Host: "shop.localhost", Target: upstream.URL, PreferredScheme: "https",
		LANShare: &LANShare{State: LANShareActive, RequestedHostname: "shop.local.", Hostname: "shop.local.", CreatedAt: time.Now()},
	}
	store := NewMemoryStore()
	if err := store.Save([]Route{route}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(Config{Store: store})
	if err := server.RegisterLANRoute(route.Ref(), "shop.local.", tls.Certificate{}); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "https://shop.local/assets/app.js", nil)
	request.Host = "shop.local"
	request.TLS = &tls.ConnectionState{ServerName: "shop.local"}
	response := httptest.NewRecorder()
	server.LANHandler().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
	if got := <-upstreamHost; got != "shop.localhost" {
		t.Fatalf("upstream Host = %q", got)
	}
	stored, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if stored[0].LANShare.LastConnectedAt.IsZero() {
		t.Fatal("LAN connection time was not recorded")
	}
}

func TestLANHandlerRejectsUnsharedLocalhostUnknownAndMismatch(t *testing.T) {
	shared := Route{ID: "shared", Generation: 1, State: RouteStateActive, Host: "shop.localhost", Target: "http://127.0.0.1:5173", PreferredScheme: "https",
		LANShare: &LANShare{State: LANShareActive, RequestedHostname: "shop.local.", Hostname: "shop.local.", CreatedAt: time.Now()},
	}
	unshared := Route{ID: "private", Generation: 1, State: RouteStateActive, Host: "private.localhost", Target: "http://127.0.0.1:3000"}
	store := NewMemoryStore()
	if err := store.Save([]Route{shared, unshared}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(Config{Store: store})
	if err := server.RegisterLANRoute(shared.Ref(), "shop.local", tls.Certificate{}); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name string
		host string
		sni  string
		want int
	}{
		{name: "localhost", host: "private.localhost", sni: "private.localhost", want: http.StatusNotFound},
		{name: "unknown", host: "unknown.local", sni: "unknown.local", want: http.StatusNotFound},
		{name: "host SNI mismatch", host: "shop.local", sni: "other.local", want: http.StatusMisdirectedRequest},
		{name: "missing SNI", host: "shop.local", want: http.StatusMisdirectedRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "https://"+test.host+"/", nil)
			request.Host = test.host
			request.TLS = &tls.ConnectionState{ServerName: test.sni}
			response := httptest.NewRecorder()
			server.LANHandler().ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d", response.Code, test.want)
			}
		})
	}
}

func TestLANHTTPHandlerRedirectsOnlyRegisteredActiveHostname(t *testing.T) {
	route := Route{ID: "route-1", Generation: 1, State: RouteStateActive, Host: "shop.localhost", Target: "http://127.0.0.1:5173",
		LANShare: &LANShare{State: LANShareActive, RequestedHostname: "shop.local.", Hostname: "shop.local.", CreatedAt: time.Now()},
	}
	store := NewMemoryStore()
	if err := store.Save([]Route{route}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(Config{Store: store})
	if err := server.RegisterLANRoute(route.Ref(), "shop.local", tls.Certificate{}); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "http://shop.local/path?q=1", nil)
	request.Host = "shop.local"
	response := httptest.NewRecorder()
	server.LANHTTPHandler().ServeHTTP(response, request)
	if response.Code != http.StatusPermanentRedirect || response.Header().Get("Location") != "https://shop.local/path?q=1" {
		t.Fatalf("redirect = %d %q", response.Code, response.Header().Get("Location"))
	}

	unknown := httptest.NewRequest(http.MethodGet, "http://private.localhost/", nil)
	unknown.Host = "private.localhost"
	unknownResponse := httptest.NewRecorder()
	server.LANHTTPHandler().ServeHTTP(unknownResponse, unknown)
	if unknownResponse.Code != http.StatusNotFound {
		t.Fatalf("unknown status = %d", unknownResponse.Code)
	}
}

func TestLANRegistryReturnsOnlyExactSNICertificate(t *testing.T) {
	route := Route{ID: "route-1", Generation: 1, State: RouteStateActive, Host: "shop.localhost", Target: "http://127.0.0.1:5173",
		LANShare: &LANShare{State: LANShareActive, RequestedHostname: "shop.local.", Hostname: "shop.local.", CreatedAt: time.Now()},
	}
	store := NewMemoryStore()
	if err := store.Save([]Route{route}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(Config{Store: store})
	certificate := tls.Certificate{Certificate: [][]byte{{1, 2, 3}}}
	if err := server.RegisterLANRoute(route.Ref(), "Shop.Local", certificate); err != nil {
		t.Fatal(err)
	}
	got, err := server.LANTLSConfig().GetCertificate(&tls.ClientHelloInfo{ServerName: "shop.local"})
	if err != nil || len(got.Certificate) != 1 {
		t.Fatalf("certificate = %#v, error = %v", got, err)
	}
	for _, name := range []string{"", "private.localhost", "unknown.local"} {
		if _, err := server.LANTLSConfig().GetCertificate(&tls.ClientHelloInfo{ServerName: name}); err == nil {
			t.Fatalf("GetCertificate(%q) error = nil", name)
		}
	}
	route.LANShare.State = LANShareSuspended
	if err := store.Save([]Route{route}); err != nil {
		t.Fatal(err)
	}
	if _, err := server.LANTLSConfig().GetCertificate(&tls.ClientHelloInfo{ServerName: "shop.local"}); err == nil {
		t.Fatal("inactive LAN share returned a certificate")
	}
}
