package router

import (
	"container/list"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	localcert "github.com/roie/gohere/internal/cert"
	"golang.org/x/sync/singleflight"
)

type tlsIdentityKind uint8

const (
	tlsIdentityDiagnostic tlsIdentityKind = iota + 1
	tlsIdentityRoute

	localTLSCacheCapacity = 128
	localTLSCacheTTL      = time.Hour
)

type localhostCertificateStore interface {
	LoadCA() (localcert.CA, error)
	IssueLocalhostCert(localcert.CA, string, time.Time) (tls.Certificate, error)
}

type tlsAuthorizationKey struct {
	kind          tlsIdentityKind
	host          string
	routeID       string
	generation    uint64
	caFingerprint string
}

type localTLSPolicy struct {
	server       *Server
	certificates localhostCertificateStore
	cache        *localTLSCache
	now          func() time.Time
}

func newLocalTLSPolicy(server *Server, certificates localhostCertificateStore) *localTLSPolicy {
	return &localTLSPolicy{
		server:       server,
		certificates: certificates,
		cache:        newLocalTLSCache(localTLSCacheCapacity, localTLSCacheTTL),
		now:          time.Now,
	}
}

func newLocalTLSConfig(
	base *tls.Config,
	server *Server,
	certificates localhostCertificateStore,
) (*tls.Config, error) {
	if base == nil {
		return nil, errors.New("HTTPS TLS configuration is required")
	}
	if server == nil {
		return nil, errors.New("HTTPS router is required")
	}
	if certificates == nil {
		return nil, errors.New("HTTPS certificate store is required")
	}

	policy := newLocalTLSPolicy(server, certificates)
	configured := base.Clone()
	baseVerify := configured.VerifyConnection
	configured.Certificates = nil
	configured.NameToCertificate = nil
	configured.GetCertificate = nil
	configured.SessionTicketsDisabled = true
	configured.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		if hello == nil {
			return nil, errors.New("TLS ClientHello is required")
		}
		key, ca, err := policy.authorize(hello.ServerName)
		if err != nil {
			return nil, err
		}

		selected := configured.Clone()
		selected.GetConfigForClient = nil
		selected.Certificates = nil
		selected.NameToCertificate = nil
		selected.SessionTicketsDisabled = true
		selected.GetCertificate = func(current *tls.ClientHelloInfo) (*tls.Certificate, error) {
			return policy.certificateFor(current, key, ca)
		}
		selected.VerifyConnection = func(state tls.ConnectionState) error {
			host, err := normalizeLocalSNI(state.ServerName)
			if err != nil || host != key.host {
				return errors.New("TLS server name no longer matches authorization")
			}
			if err := policy.revalidate(key); err != nil {
				return err
			}
			if baseVerify != nil {
				return baseVerify(state)
			}
			return nil
		}
		return selected, nil
	}
	return configured, nil
}

func (p *localTLSPolicy) authorize(rawHost string) (tlsAuthorizationKey, localcert.CA, error) {
	host, err := normalizeLocalSNI(rawHost)
	if err != nil {
		return tlsAuthorizationKey{}, localcert.CA{}, err
	}
	ca, err := p.certificates.LoadCA()
	if err != nil {
		return tlsAuthorizationKey{}, localcert.CA{}, fmt.Errorf(
			"HTTPS certificate authority is unavailable; run gohere setup: %w",
			err,
		)
	}
	caFingerprint, err := certificateAuthorityFingerprint(ca)
	if err != nil {
		return tlsAuthorizationKey{}, localcert.CA{}, err
	}

	if host == "localhost" {
		return tlsAuthorizationKey{
			kind:          tlsIdentityDiagnostic,
			host:          host,
			caFingerprint: caFingerprint,
		}, ca, nil
	}

	route, found, err := p.server.routeForHost(host)
	if err != nil {
		return tlsAuthorizationKey{}, localcert.CA{}, fmt.Errorf("resolve TLS route: %w", err)
	}
	if !found {
		return tlsAuthorizationKey{}, localcert.CA{}, fmt.Errorf("no current route owns TLS hostname %q", host)
	}
	if route.ID == "" || route.Generation == 0 {
		return tlsAuthorizationKey{}, localcert.CA{}, fmt.Errorf("TLS route %q has no stable identity", host)
	}
	now := p.now()
	switch route.EffectiveState() {
	case RouteStatePending:
		if RouteReservationExpired(route, now) {
			return tlsAuthorizationKey{}, localcert.CA{}, fmt.Errorf("TLS route reservation for %q expired", host)
		}
	case RouteStateActive:
		if RouteLeaseExpired(route, now) {
			return tlsAuthorizationKey{}, localcert.CA{}, fmt.Errorf("TLS route lease for %q expired", host)
		}
	default:
		return tlsAuthorizationKey{}, localcert.CA{}, fmt.Errorf("TLS route %q is not currently owned", host)
	}

	return tlsAuthorizationKey{
		kind:          tlsIdentityRoute,
		host:          host,
		routeID:       route.ID,
		generation:    route.Generation,
		caFingerprint: caFingerprint,
	}, ca, nil
}

func (p *localTLSPolicy) revalidate(want tlsAuthorizationKey) error {
	got, _, err := p.authorize(want.host)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("TLS authorization for %q changed", want.host)
	}
	return nil
}

func (p *localTLSPolicy) certificateFor(
	hello *tls.ClientHelloInfo,
	key tlsAuthorizationKey,
	ca localcert.CA,
) (*tls.Certificate, error) {
	if hello == nil {
		return nil, errors.New("TLS ClientHello is required")
	}
	host, err := normalizeLocalSNI(hello.ServerName)
	if err != nil || host != key.host {
		return nil, errors.New("TLS server name no longer matches authorization")
	}
	if err := p.revalidate(key); err != nil {
		return nil, err
	}
	if certificate := p.cache.get(key); certificate != nil {
		return certificate, nil
	}

	value, err, _ := p.cache.group.Do(key.singleflightKey(), func() (any, error) {
		if err := p.revalidate(key); err != nil {
			return nil, err
		}
		if certificate := p.cache.get(key); certificate != nil {
			return certificate, nil
		}
		certificate, err := p.certificates.IssueLocalhostCert(ca, key.host, p.now())
		if err != nil {
			return nil, err
		}
		if err := p.revalidate(key); err != nil {
			return nil, err
		}
		p.cache.put(key, &certificate)
		return &certificate, nil
	})
	if err != nil {
		return nil, err
	}
	if err := p.revalidate(key); err != nil {
		return nil, err
	}
	certificate, ok := value.(*tls.Certificate)
	if !ok || certificate == nil {
		return nil, errors.New("TLS certificate issuer returned an invalid result")
	}
	return certificate, nil
}

func (key tlsAuthorizationKey) singleflightKey() string {
	return fmt.Sprintf(
		"%d\x00%s\x00%s\x00%d\x00%s",
		key.kind,
		key.host,
		key.routeID,
		key.generation,
		key.caFingerprint,
	)
}

func certificateAuthorityFingerprint(ca localcert.CA) (string, error) {
	if ca.Cert == nil || len(ca.Cert.Raw) == 0 {
		return "", errors.New("HTTPS certificate authority is incomplete")
	}
	sum := sha256.Sum256(ca.Cert.Raw)
	return hex.EncodeToString(sum[:]), nil
}

func normalizeLocalSNI(raw string) (string, error) {
	host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
	if host == "" || strings.Contains(host, ":") || net.ParseIP(host) != nil {
		return "", fmt.Errorf("invalid localhost SNI %q", raw)
	}
	if host != "localhost" && !strings.HasSuffix(host, ".localhost") {
		return "", fmt.Errorf("invalid localhost SNI %q", raw)
	}
	if len(host) > 253 {
		return "", fmt.Errorf("invalid localhost SNI %q", raw)
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("invalid localhost SNI %q", raw)
		}
		for _, character := range label {
			if (character >= 'a' && character <= 'z') ||
				(character >= '0' && character <= '9') ||
				character == '-' {
				continue
			}
			return "", fmt.Errorf("invalid localhost SNI %q", raw)
		}
	}
	return host, nil
}

type localTLSCacheEntry struct {
	key       tlsAuthorizationKey
	cert      *tls.Certificate
	expiresAt time.Time
}

type localTLSCache struct {
	mu       sync.Mutex
	entries  map[tlsAuthorizationKey]*list.Element
	lru      *list.List
	capacity int
	ttl      time.Duration
	now      func() time.Time
	group    singleflight.Group
}

func newLocalTLSCache(capacity int, ttl time.Duration) *localTLSCache {
	return &localTLSCache{
		entries:  make(map[tlsAuthorizationKey]*list.Element),
		lru:      list.New(),
		capacity: capacity,
		ttl:      ttl,
		now:      time.Now,
	}
}

func (cache *localTLSCache) get(key tlsAuthorizationKey) *tls.Certificate {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	element := cache.entries[key]
	if element == nil {
		return nil
	}
	entry := element.Value.(*localTLSCacheEntry)
	if !cache.now().Before(entry.expiresAt) {
		cache.remove(element)
		return nil
	}
	cache.lru.MoveToFront(element)
	return entry.cert
}

func (cache *localTLSCache) put(key tlsAuthorizationKey, certificate *tls.Certificate) {
	if certificate == nil {
		return
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()

	expiresAt := cache.now().Add(cache.ttl)
	if certificate.Leaf != nil && certificate.Leaf.NotAfter.Before(expiresAt) {
		expiresAt = certificate.Leaf.NotAfter
	}
	if existing := cache.entries[key]; existing != nil {
		entry := existing.Value.(*localTLSCacheEntry)
		entry.cert = certificate
		entry.expiresAt = expiresAt
		cache.lru.MoveToFront(existing)
		return
	}
	element := cache.lru.PushFront(&localTLSCacheEntry{
		key:       key,
		cert:      certificate,
		expiresAt: expiresAt,
	})
	cache.entries[key] = element
	for cache.lru.Len() > cache.capacity {
		cache.remove(cache.lru.Back())
	}
}

func (cache *localTLSCache) remove(element *list.Element) {
	if element == nil {
		return
	}
	entry := element.Value.(*localTLSCacheEntry)
	delete(cache.entries, entry.key)
	cache.lru.Remove(element)
}
