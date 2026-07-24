package router

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	localcert "github.com/roie/gohere/internal/cert"
)

type fakeLocalhostCertificateStore struct {
	mu     sync.Mutex
	ca     localcert.CA
	issues int

	issueStarted  chan struct{}
	issueContinue chan struct{}
	startOnce     sync.Once
}

func (s *fakeLocalhostCertificateStore) LoadCA() (localcert.CA, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ca, nil
}

func (s *fakeLocalhostCertificateStore) IssueLocalhostCert(
	ca localcert.CA,
	host string,
	now time.Time,
) (tls.Certificate, error) {
	s.mu.Lock()
	s.issues++
	started := s.issueStarted
	continued := s.issueContinue
	s.mu.Unlock()
	if started != nil {
		s.startOnce.Do(func() { close(started) })
	}
	if continued != nil {
		<-continued
	}
	return (localcert.Store{}).IssueLocalhostCert(ca, host, now)
}

func (s *fakeLocalhostCertificateStore) issueCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.issues
}

func (s *fakeLocalhostCertificateStore) setCA(ca localcert.CA) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ca = ca
}

func testLocalTLSCA(t *testing.T) localcert.CA {
	t.Helper()
	ca, err := (localcert.Store{StateDir: t.TempDir()}).EnsureCA()
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func TestLocalTLSAuthorizationMatrix(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		host    string
		route   *Route
		wantErr bool
	}{
		{name: "diagnostic localhost", host: "localhost"},
		{
			name: "pending",
			host: "app.localhost",
			route: &Route{
				ID: "r1", Generation: 1, State: RouteStatePending, Host: "app.localhost",
				ReservationExpiresAt: now.Add(time.Minute),
			},
		},
		{
			name: "active https",
			host: "app.localhost",
			route: &Route{
				ID: "r1", Generation: 1, State: RouteStateActive, Host: "app.localhost",
				PreferredScheme: "https", LeaseExpiresAt: now.Add(time.Minute),
			},
		},
		{
			name: "active http",
			host: "app.localhost",
			route: &Route{
				ID: "r1", Generation: 1, State: RouteStateActive, Host: "app.localhost",
				PreferredScheme: "http", LeaseExpiresAt: now.Add(time.Minute),
			},
		},
		{
			name: "legacy zero lease",
			host: "app.localhost",
			route: &Route{
				ID: "r1", Generation: 1, State: RouteStateActive, Host: "app.localhost",
			},
		},
		{name: "missing sni", wantErr: true},
		{name: "ip literal", host: "127.0.0.1", wantErr: true},
		{name: "unknown", host: "unknown.localhost", wantErr: true},
		{
			name: "expired pending",
			host: "app.localhost",
			route: &Route{
				ID: "r1", Generation: 1, State: RouteStatePending, Host: "app.localhost",
				ReservationExpiresAt: now.Add(-time.Second),
			},
			wantErr: true,
		},
		{
			name: "expired active",
			host: "app.localhost",
			route: &Route{
				ID: "r1", Generation: 1, State: RouteStateActive, Host: "app.localhost",
				LeaseExpiresAt: now.Add(-time.Second),
			},
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := NewMemoryStore()
			if test.route != nil {
				if err := store.Save([]Route{*test.route}); err != nil {
					t.Fatal(err)
				}
			}
			certificates := &fakeLocalhostCertificateStore{ca: testLocalTLSCA(t)}
			policy := newLocalTLSPolicy(
				NewServer(Config{Token: "secret", Store: store}),
				certificates,
			)
			policy.now = func() time.Time { return now }

			key, _, err := policy.authorize(test.host)
			if test.wantErr {
				if err == nil {
					t.Fatalf("authorize(%q) succeeded with key %#v", test.host, key)
				}
				return
			}
			if err != nil {
				t.Fatalf("authorize(%q): %v", test.host, err)
			}
			if key.host != test.host {
				t.Fatalf("key host = %q, want %q", key.host, test.host)
			}
			if test.route != nil &&
				(key.kind != tlsIdentityRoute ||
					key.routeID != test.route.ID ||
					key.generation != test.route.Generation) {
				t.Fatalf("route key = %#v", key)
			}
		})
	}
}

func TestLocalTLSConfigRejectsMissingSNIAndPreservesBasePolicy(t *testing.T) {
	store := NewMemoryStore()
	if err := store.Save([]Route{{
		ID: "route-1", Generation: 1, State: RouteStateActive, Host: "app.localhost",
	}}); err != nil {
		t.Fatal(err)
	}
	certificates := &fakeLocalhostCertificateStore{ca: testLocalTLSCA(t)}
	base := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		MaxVersion:   tls.VersionTLS13,
		NextProtos:   []string{"h2", "http/1.1"},
		CipherSuites: []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256},
		Certificates: []tls.Certificate{{}},
		NameToCertificate: map[string]*tls.Certificate{
			"fallback": {},
		},
	}

	configured, err := newLocalTLSConfig(
		base,
		NewServer(Config{Token: "secret", Store: store}),
		certificates,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(configured.Certificates) != 0 || len(configured.NameToCertificate) != 0 {
		t.Fatal("configured TLS retained fallback certificates")
	}
	if !configured.SessionTicketsDisabled {
		t.Fatal("base TLS config permits session tickets")
	}
	if _, err := configured.GetConfigForClient(&tls.ClientHelloInfo{}); err == nil {
		t.Fatal("missing SNI was accepted")
	}

	selected, err := configured.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "APP.LOCALHOST."})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.Certificates) != 0 || len(selected.NameToCertificate) != 0 {
		t.Fatal("selected TLS retained fallback certificates")
	}
	if !selected.SessionTicketsDisabled {
		t.Fatal("selected TLS config permits session tickets")
	}
	if selected.GetCertificate == nil || selected.GetConfigForClient != nil {
		t.Fatal("selected callbacks are not route-bound")
	}
	if selected.MinVersion != base.MinVersion ||
		selected.MaxVersion != base.MaxVersion ||
		len(selected.NextProtos) != len(base.NextProtos) ||
		len(selected.CipherSuites) != len(base.CipherSuites) {
		t.Fatalf("selected config did not preserve base policy: %#v", selected)
	}
	if _, err := selected.GetCertificate(&tls.ClientHelloInfo{ServerName: "app.localhost"}); err != nil {
		t.Fatal(err)
	}
	if certificates.issueCount() != 1 {
		t.Fatalf("issuer count = %d, want 1", certificates.issueCount())
	}
}

func TestLocalTLSCachedCertificateDoesNotAuthorizeRemovedRoute(t *testing.T) {
	store := NewMemoryStore()
	if err := store.Save([]Route{{
		ID: "route-1", Generation: 1, State: RouteStateActive, Host: "app.localhost",
	}}); err != nil {
		t.Fatal(err)
	}
	certificates := &fakeLocalhostCertificateStore{ca: testLocalTLSCA(t)}
	configured, err := newLocalTLSConfig(
		&tls.Config{MinVersion: tls.VersionTLS12},
		NewServer(Config{Token: "secret", Store: store}),
		certificates,
	)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := configured.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "app.localhost"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := selected.GetCertificate(&tls.ClientHelloInfo{ServerName: "app.localhost"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := selected.GetCertificate(&tls.ClientHelloInfo{ServerName: "app.localhost"}); err == nil {
		t.Fatal("cached certificate authorized a removed route")
	}
	if certificates.issueCount() != 1 {
		t.Fatalf("issuer count = %d, want cached result before removal", certificates.issueCount())
	}
}

func TestLocalTLSRejectsRouteReplacementDuringIssuance(t *testing.T) {
	store := NewMemoryStore()
	if err := store.Save([]Route{{
		ID: "route-1", Generation: 1, State: RouteStateActive, Host: "app.localhost",
	}}); err != nil {
		t.Fatal(err)
	}
	certificates := &fakeLocalhostCertificateStore{
		ca:            testLocalTLSCA(t),
		issueStarted:  make(chan struct{}),
		issueContinue: make(chan struct{}),
	}
	configured, err := newLocalTLSConfig(
		&tls.Config{MinVersion: tls.VersionTLS12},
		NewServer(Config{Token: "secret", Store: store}),
		certificates,
	)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := configured.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "app.localhost"})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := selected.GetCertificate(&tls.ClientHelloInfo{ServerName: "app.localhost"})
		result <- err
	}()
	select {
	case <-certificates.issueStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("certificate issuance did not start")
	}
	if err := store.Save([]Route{{
		ID: "route-2", Generation: 2, State: RouteStateActive, Host: "app.localhost",
	}}); err != nil {
		t.Fatal(err)
	}
	close(certificates.issueContinue)
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("issuance returned a certificate for a replaced route")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("certificate issuance did not finish")
	}
}

func TestLocalTLSCoalescesConcurrentIssuance(t *testing.T) {
	store := NewMemoryStore()
	if err := store.Save([]Route{{
		ID: "route-1", Generation: 1, State: RouteStateActive, Host: "app.localhost",
	}}); err != nil {
		t.Fatal(err)
	}
	certificates := &fakeLocalhostCertificateStore{
		ca:            testLocalTLSCA(t),
		issueStarted:  make(chan struct{}),
		issueContinue: make(chan struct{}),
	}
	configured, err := newLocalTLSConfig(
		&tls.Config{MinVersion: tls.VersionTLS12},
		NewServer(Config{Token: "secret", Store: store}),
		certificates,
	)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := configured.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "app.localhost"})
	if err != nil {
		t.Fatal(err)
	}

	const callers = 12
	start := make(chan struct{})
	results := make(chan error, callers)
	for range callers {
		go func() {
			<-start
			_, err := selected.GetCertificate(&tls.ClientHelloInfo{ServerName: "app.localhost"})
			results <- err
		}()
	}
	close(start)
	select {
	case <-certificates.issueStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("certificate issuance did not start")
	}
	close(certificates.issueContinue)
	for range callers {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if certificates.issueCount() != 1 {
		t.Fatalf("issuer count = %d, want 1", certificates.issueCount())
	}
}

func TestLocalTLSRejectsCapturedKeyAfterCARotation(t *testing.T) {
	store := NewMemoryStore()
	if err := store.Save([]Route{{
		ID: "route-1", Generation: 1, State: RouteStateActive, Host: "app.localhost",
	}}); err != nil {
		t.Fatal(err)
	}
	certificates := &fakeLocalhostCertificateStore{ca: testLocalTLSCA(t)}
	configured, err := newLocalTLSConfig(
		&tls.Config{MinVersion: tls.VersionTLS12},
		NewServer(Config{Token: "secret", Store: store}),
		certificates,
	)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := configured.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "app.localhost"})
	if err != nil {
		t.Fatal(err)
	}
	certificates.setCA(testLocalTLSCA(t))
	if _, err := selected.GetCertificate(&tls.ClientHelloInfo{ServerName: "app.localhost"}); err == nil {
		t.Fatal("captured authorization survived CA rotation")
	}
	if certificates.issueCount() != 0 {
		t.Fatalf("issuer count = %d, want 0", certificates.issueCount())
	}
}

func TestLocalTLSVerifyConnectionRejectsRouteRemovedAfterCertificateSelection(t *testing.T) {
	store := NewMemoryStore()
	if err := store.Save([]Route{{
		ID: "route-1", Generation: 1, State: RouteStateActive, Host: "app.localhost",
	}}); err != nil {
		t.Fatal(err)
	}
	certificates := &fakeLocalhostCertificateStore{ca: testLocalTLSCA(t)}
	configured, err := newLocalTLSConfig(
		&tls.Config{MinVersion: tls.VersionTLS12},
		NewServer(Config{Token: "secret", Store: store}),
		certificates,
	)
	if err != nil {
		t.Fatal(err)
	}
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
		connection.Close()
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
			ServerName: "app.localhost",
		})
		_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
		err = connection.Handshake()
		if err == nil {
			var applicationData [1]byte
			_, err = io.ReadFull(connection, applicationData[:])
		}
		clientResult <- err
		connection.Close()
	}()

	select {
	case <-certificateSelected:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not select a certificate")
	}
	if err := store.Save(nil); err != nil {
		t.Fatal(err)
	}
	close(continueHandshake)
	if err := <-serverResult; err == nil {
		t.Fatal("server handshake accepted a route removed after certificate selection")
	}
	if err := <-clientResult; err == nil {
		t.Fatal("client handshake succeeded after route removal")
	}
}

func TestLocalTLSCacheBoundsAndExpiresEntries(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	cache := newLocalTLSCache(2, time.Hour)
	cache.now = func() time.Time { return now }
	certificate := func(notAfter time.Time) *tls.Certificate {
		return &tls.Certificate{Leaf: &x509.Certificate{NotAfter: notAfter}}
	}
	key := func(index int) tlsAuthorizationKey {
		return tlsAuthorizationKey{
			kind: tlsIdentityRoute, host: fmt.Sprintf("app-%d.localhost", index),
			routeID: fmt.Sprintf("route-%d", index), generation: 1, caFingerprint: "ca",
		}
	}

	cache.put(key(1), certificate(now.Add(2*time.Hour)))
	cache.put(key(2), certificate(now.Add(30*time.Minute)))
	cache.put(key(3), certificate(now.Add(2*time.Hour)))
	if cache.get(key(1)) != nil {
		t.Fatal("least-recently-used entry survived capacity eviction")
	}
	if cache.get(key(3)) == nil {
		t.Fatal("newest entry was evicted")
	}
	now = now.Add(31 * time.Minute)
	if cache.get(key(2)) != nil {
		t.Fatal("entry survived certificate expiry")
	}
	now = now.Add(30 * time.Minute)
	if cache.get(key(3)) != nil {
		t.Fatal("entry survived cache TTL")
	}
}

type recordingClientSessionCache struct {
	inner tls.ClientSessionCache
	puts  chan struct{}
}

func newRecordingClientSessionCache() *recordingClientSessionCache {
	return &recordingClientSessionCache{
		inner: tls.NewLRUClientSessionCache(8),
		puts:  make(chan struct{}, 8),
	}
}

func (cache *recordingClientSessionCache) Get(key string) (*tls.ClientSessionState, bool) {
	return cache.inner.Get(key)
}

func (cache *recordingClientSessionCache) Put(key string, state *tls.ClientSessionState) {
	cache.inner.Put(key, state)
	if state != nil {
		select {
		case cache.puts <- struct{}{}:
		default:
		}
	}
}

func TestLocalTLSSessionTicketsDisabled(t *testing.T) {
	for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		t.Run(tls.VersionName(version), func(t *testing.T) {
			ca := testLocalTLSCA(t)
			leaf, err := (localcert.Store{}).IssueLocalhostCert(
				ca,
				"app.localhost",
				time.Now(),
			)
			if err != nil {
				t.Fatal(err)
			}
			var ticketKey [32]byte
			copy(ticketKey[:], []byte("gohere route authorization test"))

			controlConfig := &tls.Config{
				MinVersion:       version,
				MaxVersion:       version,
				Certificates:     []tls.Certificate{leaf},
				SessionTicketKey: ticketKey,
			}
			controlAddr, controlDone := startTLSTestServer(t, controlConfig, 2)
			controlCache := newRecordingClientSessionCache()
			controlClient := &tls.Config{
				MinVersion:         version,
				MaxVersion:         version,
				InsecureSkipVerify: true,
				ServerName:         "app.localhost",
				ClientSessionCache: controlCache,
			}

			firstState := dialTLSTestServer(t, controlAddr, controlClient)
			if firstState.DidResume {
				t.Fatal("first control handshake unexpectedly resumed")
			}
			select {
			case <-controlCache.puts:
			case <-time.After(2 * time.Second):
				t.Fatal("positive-control client did not process NewSessionTicket")
			}
			controlState := dialTLSTestServer(t, controlAddr, controlClient)
			if !controlState.DidResume {
				t.Fatal("positive control did not resume")
			}
			if err := <-controlDone; err != nil {
				t.Fatal(err)
			}

			routeStore := NewMemoryStore()
			if err := routeStore.Save([]Route{{
				ID: "route-1", Generation: 1, State: RouteStateActive, Host: "app.localhost",
			}}); err != nil {
				t.Fatal(err)
			}
			certificates := &fakeLocalhostCertificateStore{ca: ca}
			productionConfig, err := newLocalTLSConfig(
				&tls.Config{
					MinVersion:       version,
					MaxVersion:       version,
					SessionTicketKey: ticketKey,
				},
				NewServer(Config{Token: "secret", Store: routeStore}),
				certificates,
			)
			if err != nil {
				t.Fatal(err)
			}
			productionAddr, productionDone := startTLSTestServer(t, productionConfig, 2)

			productionState := dialTLSTestServer(t, productionAddr, controlClient)
			if productionState.DidResume {
				t.Fatal("production TLS resumed despite disabled tickets")
			}

			freshCache := newRecordingClientSessionCache()
			freshClient := controlClient.Clone()
			freshClient.ClientSessionCache = freshCache
			freshState := dialTLSTestServer(t, productionAddr, freshClient)
			if freshState.DidResume {
				t.Fatal("fresh production handshake unexpectedly resumed")
			}
			select {
			case <-freshCache.puts:
				t.Fatal("production TLS issued a session ticket")
			case <-time.After(100 * time.Millisecond):
			}
			if err := <-productionDone; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func startTLSTestServer(
	t *testing.T,
	config *tls.Config,
	connections int,
) (string, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		defer listener.Close()
		for range connections {
			rawConnection, err := listener.Accept()
			if err != nil {
				done <- err
				return
			}
			connection := tls.Server(rawConnection, config)
			if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				rawConnection.Close()
				done <- err
				return
			}
			if err := connection.Handshake(); err != nil {
				rawConnection.Close()
				done <- err
				return
			}
			if _, err := connection.Write([]byte{1}); err != nil {
				rawConnection.Close()
				done <- err
				return
			}
			if err := connection.Close(); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	return listener.Addr().String(), done
}

func dialTLSTestServer(t *testing.T, address string, config *tls.Config) tls.ConnectionState {
	t.Helper()
	connection, err := tls.Dial("tcp", address, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		connection.Close()
		t.Fatal(err)
	}
	var applicationData [1]byte
	if _, err := io.ReadFull(connection, applicationData[:]); err != nil {
		connection.Close()
		t.Fatal(err)
	}
	state := connection.ConnectionState()
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	return state
}
