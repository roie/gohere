package router

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	localcert "github.com/roie/gohere/internal/cert"
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
	config := server.LANTLSConfig()
	if !config.SessionTicketsDisabled {
		t.Fatal("LAN TLS permits session resumption across route or CA changes")
	}
	got, err := lanCertificateForServer(server, "shop.local")
	if err != nil || len(got.Certificate) != 1 {
		t.Fatalf("certificate = %#v, error = %v", got, err)
	}
	for _, name := range []string{"", "private.localhost", "unknown.local"} {
		if _, err := lanCertificateForServer(server, name); err == nil {
			t.Fatalf("GetCertificate(%q) error = nil", name)
		}
	}
	route.LANShare.State = LANShareSuspended
	if err := store.Save([]Route{route}); err != nil {
		t.Fatal(err)
	}
	if _, err := lanCertificateForServer(server, "shop.local"); err == nil {
		t.Fatal("inactive LAN share returned a certificate")
	}
}

func TestLANRegistryRejectsCertificateFromPreviousCA(t *testing.T) {
	route := Route{
		ID: "route-1", Generation: 1, State: RouteStateActive,
		Host: "shop.localhost", Target: "http://127.0.0.1:5173",
		LANShare: &LANShare{
			State: LANShareActive, RequestedHostname: "shop.local.",
			Hostname: "shop.local.", CreatedAt: time.Now(),
		},
	}
	store := NewMemoryStore()
	if err := store.Save([]Route{route}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(Config{Store: store})
	server.lanMu.Lock()
	server.currentLANCAFingerprint = func() (string, error) {
		return lanCAFingerprint([]byte("new-ca")), nil
	}
	server.lanMu.Unlock()
	certificate := tls.Certificate{
		Certificate: [][]byte{[]byte("leaf"), []byte("old-ca")},
	}
	if err := server.RegisterLANRoute(route.Ref(), "shop.local", certificate); err != nil {
		t.Fatal(err)
	}

	if _, err := lanCertificateForServer(server, "shop.local"); err == nil {
		t.Fatal("LAN TLS served a certificate from the previous CA")
	}
}

func TestLANTLSVerifyConnectionRejectsCARotationAfterCertificateSelection(t *testing.T) {
	route := Route{
		ID: "route-1", Generation: 1, State: RouteStateActive,
		Host: "shop.localhost", Target: "http://127.0.0.1:5173",
		LANShare: &LANShare{
			State: LANShareActive, RequestedHostname: "shop.local.",
			Hostname: "shop.local.", CreatedAt: time.Now(),
		},
	}
	store := NewMemoryStore()
	if err := store.Save([]Route{route}); err != nil {
		t.Fatal(err)
	}
	ca := testLocalTLSCA(t)
	certificate, err := (localcert.Store{}).IssueLANHostCert(ca, "shop.local", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(Config{Store: store})
	var fingerprintMu sync.RWMutex
	currentFingerprint := lanCAFingerprint(ca.Cert.Raw)
	server.currentLANCAFingerprint = func() (string, error) {
		fingerprintMu.RLock()
		defer fingerprintMu.RUnlock()
		return currentFingerprint, nil
	}
	if err := server.RegisterLANRoute(route.Ref(), "shop.local", certificate); err != nil {
		t.Fatal(err)
	}

	configured := server.LANTLSConfig()
	certificateSelected := make(chan struct{})
	continueHandshake := make(chan struct{})
	originalGetConfig := configured.GetConfigForClient
	configured.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		selected, err := originalGetConfig(hello)
		if err != nil {
			return nil, err
		}
		originalGetCertificate := selected.GetCertificate
		selected.GetCertificate = func(current *tls.ClientHelloInfo) (*tls.Certificate, error) {
			certificate, err := originalGetCertificate(current)
			close(certificateSelected)
			<-continueHandshake
			return certificate, err
		}
		return selected, nil
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverResult := make(chan error, 1)
	go func() {
		rawConnection, err := listener.Accept()
		if err != nil {
			serverResult <- err
			return
		}
		connection := tls.Server(rawConnection, configured)
		_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
		err = connection.Handshake()
		if err == nil {
			_, err = connection.Write([]byte{1})
		}
		serverResult <- err
		_ = connection.Close()
	}()
	clientResult := make(chan error, 1)
	go func() {
		rawConnection, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			clientResult <- err
			return
		}
		connection := tls.Client(rawConnection, &tls.Config{
			MinVersion: tls.VersionTLS12, InsecureSkipVerify: true,
			ServerName: "shop.local",
		})
		_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
		err = connection.Handshake()
		if err == nil {
			var applicationData [1]byte
			_, err = io.ReadFull(connection, applicationData[:])
		}
		clientResult <- err
		_ = connection.Close()
	}()

	select {
	case <-certificateSelected:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not select a LAN certificate")
	}
	fingerprintMu.Lock()
	currentFingerprint = lanCAFingerprint([]byte("rotated-ca"))
	fingerprintMu.Unlock()
	close(continueHandshake)
	if err := <-serverResult; err == nil {
		t.Fatal("server handshake accepted a CA rotated after certificate selection")
	}
	if err := <-clientResult; err == nil {
		t.Fatal("client received application data after CA rotation")
	}
}

func lanCertificateForServer(server *Server, hostname string) (*tls.Certificate, error) {
	configured := server.LANTLSConfig()
	selected, err := configured.GetConfigForClient(&tls.ClientHelloInfo{ServerName: hostname})
	if err != nil {
		return nil, err
	}
	if selected == nil || selected.GetCertificate == nil {
		return nil, tls.AlertError(80)
	}
	return selected.GetCertificate(&tls.ClientHelloInfo{ServerName: hostname})
}
