package router

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	goherecert "github.com/roie/gohere/internal/cert"
	"github.com/roie/gohere/internal/lanfirewall"
	"github.com/roie/gohere/internal/laninterface"
	"github.com/roie/gohere/internal/lanmdns"
)

type lanIngress interface {
	Close() error
}

type lanHostnameResponder interface {
	Register(context.Context, string) (lanmdns.Registration, error)
	Close() error
}

type lanTrustRegistration struct {
	token       string
	setupURL    string
	fingerprint string
}

type lanManagerConfig struct {
	store              Store
	selectInterface    func(context.Context) (laninterface.Candidate, error)
	issueCertificate   func(string, time.Time) (tls.Certificate, error)
	prepareTrust       func(laninterface.Candidate, string) (lanTrustRegistration, error)
	prepareFirewall    func(context.Context) error
	startIngress       func(context.Context, *Server, string, string) (lanIngress, error)
	newResponder       func(context.Context, lanmdns.Interface, lanmdns.Coordinator) (lanHostnameResponder, error)
	routeOwnerVerified func(Route) bool
	targetReachable    func(Route) bool
	networkStillValid  func(context.Context, laninterface.Candidate) bool
	monitorInterval    time.Duration
	now                func() time.Time
}

type LANShareResult struct {
	Hostname    string     `json:"hostname"`
	URL         string     `json:"url"`
	Address     netip.Addr `json:"address"`
	SetupURL    string     `json:"setupUrl"`
	Fingerprint string     `json:"fingerprint"`
}

type LANManager struct {
	ctx    context.Context
	cancel context.CancelFunc
	server *Server
	config lanManagerConfig

	mu               sync.Mutex
	iface            laninterface.Candidate
	ingress          lanIngress
	responder        lanHostnameResponder
	pending          map[string]RouteRef
	registrations    map[RouteRef]lanmdns.Registration
	registrationRefs map[lanmdns.RegistrationID]RouteRef
	trust            map[RouteRef]lanTrustRegistration
	monitorStarted   bool
	closeOnce        sync.Once
	closeErr         error
}

func NewLANManager(ctx context.Context, server *Server, stateDir string, routeOwnerVerified func(Route) bool) *LANManager {
	certificateStore := goherecert.Store{StateDir: stateDir}
	return newLANManager(ctx, server, lanManagerConfig{
		store: server.store,
		selectInterface: func(ctx context.Context) (laninterface.Candidate, error) {
			return laninterface.DiscoverAndSelect(ctx, nil)
		},
		issueCertificate: certificateStore.IssueEphemeralLANHostCert,
		prepareFirewall:  lanfirewall.Ensure,
		prepareTrust: func(iface laninterface.Candidate, hostname string) (lanTrustRegistration, error) {
			ca, err := certificateStore.EnsureCA()
			if err != nil {
				return lanTrustRegistration{}, err
			}
			token, err := newLANTrustToken()
			if err != nil {
				return lanTrustRegistration{}, err
			}
			fingerprint := formatLANFingerprint(ca.Cert.Raw)
			server.ConfigureLANTrust(LANTrustSession{
				Address: iface.Prefix.Addr().String(), Token: token, Hostname: hostname, Fingerprint: fingerprint,
				CACertificatePEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Cert.Raw}),
			})
			return lanTrustRegistration{
				token: token, fingerprint: fingerprint,
				setupURL: "http://" + iface.Prefix.Addr().String() + lanTrustPathPrefix + token,
			}, nil
		},
		startIngress: func(ctx context.Context, server *Server, httpAddr, httpsAddr string) (lanIngress, error) {
			return StartLANIngress(ctx, server, httpAddr, httpsAddr)
		},
		newResponder: func(ctx context.Context, iface lanmdns.Interface, coordinator lanmdns.Coordinator) (lanHostnameResponder, error) {
			return lanmdns.New(ctx, iface, coordinator)
		},
		routeOwnerVerified: routeOwnerVerified,
		targetReachable: func(route Route) bool {
			target, err := url.Parse(route.Target)
			return err == nil && validateRouteTarget(target) == nil && probeTargetReachable(target)
		},
		networkStillValid: func(ctx context.Context, selected laninterface.Candidate) bool {
			candidates, err := laninterface.Discover(ctx)
			if err != nil {
				return false
			}
			for _, candidate := range candidates {
				if candidate.Index == selected.Index && candidate.Prefix == selected.Prefix {
					return true
				}
			}
			return false
		},
		monitorInterval: 10 * time.Second,
		now:             time.Now,
	})
}

func newLANManager(ctx context.Context, server *Server, config lanManagerConfig) *LANManager {
	if ctx == nil {
		ctx = context.Background()
	}
	managerCtx, cancel := context.WithCancel(ctx)
	return &LANManager{
		ctx: managerCtx, cancel: cancel, server: server, config: config,
		pending: make(map[string]RouteRef), registrations: make(map[RouteRef]lanmdns.Registration),
		registrationRefs: make(map[lanmdns.RegistrationID]RouteRef), trust: make(map[RouteRef]lanTrustRegistration),
	}
}

func (m *LANManager) Recover(ctx context.Context) error {
	routes, err := m.config.store.Load()
	if err != nil {
		return err
	}
	for _, route := range routes {
		if route.LANShare == nil {
			continue
		}
		if route.LANShare.State == LANShareRemoving {
			_ = RemoveLANShare(m.config.store, route.Ref())
			continue
		}
		reason := ""
		switch {
		case route.EffectiveState() != RouteStateActive:
			reason = "route is not active"
		case RouteLeaseExpired(route, m.config.now()):
			reason = "route owner lease expired"
		case m.config.routeOwnerVerified == nil || !m.config.routeOwnerVerified(route):
			reason = "route ownership could not be verified"
		case m.config.targetReachable == nil || !m.config.targetReachable(route):
			reason = "route target is unreachable"
		}
		if reason != "" {
			_ = SuspendLANShare(m.config.store, route.Ref(), reason)
			continue
		}
		if _, err := m.Create(ctx, route.Ref()); err != nil {
			_ = SuspendLANShare(m.config.store, route.Ref(), err.Error())
		}
	}
	return nil
}

func (m *LANManager) RecoverVerified(ctx context.Context, ref RouteRef) error {
	route, err := routeForRef(m.config.store, ref)
	if err != nil || route.LANShare == nil {
		return err
	}
	if route.EffectiveState() != RouteStateActive {
		return errors.New("LAN sharing requires an active route")
	}
	if m.config.targetReachable == nil || !m.config.targetReachable(route) {
		_ = SuspendLANShare(m.config.store, ref, "route target is unreachable")
		return errors.New("LAN route target is unreachable")
	}
	_, err = m.Create(ctx, ref)
	if err != nil {
		_ = SuspendLANShare(m.config.store, ref, err.Error())
	}
	return err
}

func (m *LANManager) Create(ctx context.Context, ref RouteRef) (LANShareResult, error) {
	m.mu.Lock()
	if registration, ok := m.registrations[ref]; ok {
		hostname := registration.CurrentHostname()
		m.mu.Unlock()
		return m.resultFor(ref, hostname), nil
	}
	m.mu.Unlock()

	route, err := routeForRef(m.config.store, ref)
	if err != nil {
		return LANShareResult{}, err
	}
	if route.EffectiveState() != RouteStateActive {
		return LANShareResult{}, errors.New("LAN sharing requires an active route")
	}
	updated, err := RequestLANShare(m.config.store, ref, m.config.now())
	if err != nil {
		return LANShareResult{}, err
	}
	requested := updated.LANShare.RequestedHostname

	m.mu.Lock()
	if _, exists := m.pending[requested]; exists {
		m.mu.Unlock()
		return LANShareResult{}, fmt.Errorf("LAN share %s is already being activated", requested)
	}
	if err := m.ensureNetworkLocked(ctx); err != nil {
		m.mu.Unlock()
		_ = RemoveLANShare(m.config.store, ref)
		return LANShareResult{}, err
	}
	responder := m.responder
	m.pending[requested] = ref
	m.mu.Unlock()

	registration, err := responder.Register(ctx, requested)
	if err != nil {
		m.rollbackCreate(ref, requested)
		return LANShareResult{}, err
	}
	m.mu.Lock()
	m.registrations[ref] = registration
	m.registrationRefs[registration.ID()] = ref
	m.mu.Unlock()
	return m.resultFor(ref, registration.CurrentHostname()), nil
}

func (m *LANManager) Prepare(_ context.Context, change lanmdns.Change) error {
	m.mu.Lock()
	ref, ok := m.registrationRefs[change.Registration]
	if !ok {
		ref, ok = m.pending[change.Requested]
	}
	iface := m.iface
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("no LAN route owns mDNS registration %s", change.Registration)
	}
	certificate, err := m.config.issueCertificate(change.Proposed, m.config.now())
	if err != nil {
		return err
	}
	var trust lanTrustRegistration
	if m.config.prepareTrust != nil {
		trust, err = m.config.prepareTrust(iface, change.Proposed)
		if err != nil {
			return err
		}
	}
	if err := m.server.RegisterLANRoute(ref, change.Proposed, certificate); err != nil {
		if trust.token != "" {
			m.server.ClearLANTrust(trust.token)
		}
		return err
	}
	activation := LANActivation{
		Hostname: change.Proposed, InterfaceIndex: iface.Index, InterfaceName: iface.Name,
		Address: iface.Prefix.Addr().String(), Prefix: iface.Prefix.String(),
	}
	if err := ActivateLANShare(m.config.store, ref, activation); err != nil {
		m.server.RemoveLANHostname(ref, change.Proposed)
		if trust.token != "" {
			m.server.ClearLANTrust(trust.token)
		}
		return err
	}
	m.mu.Lock()
	previousTrust := m.trust[ref]
	if trust.token != "" {
		m.trust[ref] = trust
	}
	m.mu.Unlock()
	if previousTrust.token != "" && previousTrust.token != trust.token {
		m.server.ClearLANTrust(previousTrust.token)
	}
	if change.Previous != "" && change.Previous != change.Proposed {
		m.server.RemoveLANHostname(ref, change.Previous)
	}
	return nil
}

func (m *LANManager) Remove(ctx context.Context, ref RouteRef) error {
	m.mu.Lock()
	registration := m.registrations[ref]
	trust := m.trust[ref]
	delete(m.trust, ref)
	if registration != nil {
		delete(m.registrations, ref)
		delete(m.registrationRefs, registration.ID())
		delete(m.pending, registration.RequestedHostname())
	}
	last := len(m.registrations) == 0
	responder := m.responder
	ingress := m.ingress
	if last {
		m.responder = nil
		m.ingress = nil
		m.iface = laninterface.Candidate{}
	}
	m.mu.Unlock()

	var result error
	if registration != nil {
		result = registration.Close(ctx)
	}
	m.server.RemoveLANRoute(ref)
	if trust.token != "" {
		m.server.ClearLANTrust(trust.token)
	}
	if err := RemoveLANShare(m.config.store, ref); err != nil && !errors.Is(err, ErrRouteRefMismatch) {
		result = errors.Join(result, err)
	}
	if last {
		if responder != nil {
			result = errors.Join(result, responder.Close())
		}
		if ingress != nil {
			result = errors.Join(result, ingress.Close())
		}
	}
	return result
}

func (m *LANManager) Close() error {
	if m == nil {
		return nil
	}
	m.closeOnce.Do(func() {
		m.cancel()
		m.mu.Lock()
		registrations := m.registrations
		trust := m.trust
		m.registrations = make(map[RouteRef]lanmdns.Registration)
		m.trust = make(map[RouteRef]lanTrustRegistration)
		m.registrationRefs = make(map[lanmdns.RegistrationID]RouteRef)
		m.pending = make(map[string]RouteRef)
		responder, ingress := m.responder, m.ingress
		m.responder, m.ingress = nil, nil
		m.iface = laninterface.Candidate{}
		m.mu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		for ref, registration := range registrations {
			m.closeErr = errors.Join(m.closeErr, registration.Close(ctx))
			if registration := trust[ref]; registration.token != "" {
				m.server.ClearLANTrust(registration.token)
			}
			m.server.RemoveLANRoute(ref)
			m.closeErr = errors.Join(m.closeErr, SetLANShareState(m.config.store, ref, LANShareSuspended))
		}
		if responder != nil {
			m.closeErr = errors.Join(m.closeErr, responder.Close())
		}
		if ingress != nil {
			m.closeErr = errors.Join(m.closeErr, ingress.Close())
		}
	})
	return m.closeErr
}

func (m *LANManager) ensureNetworkLocked(ctx context.Context) error {
	if m.responder != nil && m.ingress != nil {
		return nil
	}
	iface, err := m.config.selectInterface(ctx)
	if err != nil {
		return err
	}
	if m.config.prepareFirewall != nil {
		if err := m.config.prepareFirewall(ctx); err != nil {
			return err
		}
	}
	address := iface.Prefix.Addr().String()
	ingress, err := m.config.startIngress(m.ctx, m.server, net.JoinHostPort(address, "80"), net.JoinHostPort(address, "443"))
	if err != nil {
		return err
	}
	responder, err := m.config.newResponder(m.ctx, lanmdns.Interface{Index: iface.Index, Name: iface.Name, Prefix: iface.Prefix}, m)
	if err != nil {
		_ = ingress.Close()
		return err
	}
	m.iface = iface
	m.ingress = ingress
	m.responder = responder
	if !m.monitorStarted && m.config.monitorInterval > 0 {
		m.monitorStarted = true
		go m.monitorNetwork()
	}
	return nil
}

func (m *LANManager) monitorNetwork() {
	ticker := time.NewTicker(m.config.monitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.reconcileNetwork(m.ctx)
		}
	}
}

func (m *LANManager) reconcileNetwork(ctx context.Context) {
	m.mu.Lock()
	iface := m.iface
	validator := m.config.networkStillValid
	active := m.responder != nil
	m.mu.Unlock()
	if !active || validator == nil || validator(ctx, iface) {
		return
	}
	m.mu.Lock()
	if m.responder == nil || m.iface.Index != iface.Index || m.iface.Prefix != iface.Prefix {
		m.mu.Unlock()
		return
	}
	registrations := m.registrations
	trust := m.trust
	responder, ingress := m.responder, m.ingress
	m.registrations = make(map[RouteRef]lanmdns.Registration)
	m.registrationRefs = make(map[lanmdns.RegistrationID]RouteRef)
	m.pending = make(map[string]RouteRef)
	m.trust = make(map[RouteRef]lanTrustRegistration)
	m.responder, m.ingress = nil, nil
	m.iface = laninterface.Candidate{}
	m.mu.Unlock()

	for ref, registration := range registrations {
		_ = registration.Close(ctx)
		m.server.RemoveLANRoute(ref)
		if trust := trust[ref]; trust.token != "" {
			m.server.ClearLANTrust(trust.token)
		}
		_ = SuspendLANShare(m.config.store, ref, "selected LAN interface or address changed")
	}
	if responder != nil {
		_ = responder.Close()
	}
	if ingress != nil {
		_ = ingress.Close()
	}
	for ref := range registrations {
		_ = m.RecoverVerified(ctx, ref)
	}
}

func (m *LANManager) rollbackCreate(ref RouteRef, requested string) {
	m.server.RemoveLANRoute(ref)
	_ = RemoveLANShare(m.config.store, ref)
	m.mu.Lock()
	trust := m.trust[ref]
	delete(m.trust, ref)
	delete(m.pending, requested)
	if len(m.registrations) == 0 {
		responder, ingress := m.responder, m.ingress
		m.responder, m.ingress = nil, nil
		m.iface = laninterface.Candidate{}
		m.mu.Unlock()
		if trust.token != "" {
			m.server.ClearLANTrust(trust.token)
		}
		if responder != nil {
			_ = responder.Close()
		}
		if ingress != nil {
			_ = ingress.Close()
		}
		return
	}
	m.mu.Unlock()
}

func (m *LANManager) resultFor(ref RouteRef, hostname string) LANShareResult {
	m.mu.Lock()
	address := m.iface.Prefix.Addr()
	trust := m.trust[ref]
	m.mu.Unlock()
	return LANShareResult{
		Hostname: hostname, URL: "https://" + stringsTrimFinalDot(hostname), Address: address,
		SetupURL: trust.setupURL, Fingerprint: trust.fingerprint,
	}
}

func (s *Server) handleLANShare(w http.ResponseWriter, request *http.Request) {
	if !s.authorized(request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.lanManager == nil {
		http.Error(w, "LAN sharing is unavailable", http.StatusServiceUnavailable)
		return
	}
	var ref RouteRef
	if err := json.NewDecoder(io.LimitReader(request.Body, maxAdminBodyBytes)).Decode(&ref); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch request.Method {
	case http.MethodPost:
		result, err := s.lanManager.Create(request.Context(), ref)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	case http.MethodDelete:
		if err := s.lanManager.Remove(request.Context(), ref); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func routeForRef(store Store, ref RouteRef) (Route, error) {
	routes, err := store.Load()
	if err != nil {
		return Route{}, err
	}
	index := routeRefIndex(routes, ref)
	if index < 0 {
		return Route{}, ErrRouteRefMismatch
	}
	return routes[index], nil
}

func newLANTrustToken() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func formatLANFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	encoded := strings.ToUpper(hex.EncodeToString(sum[:]))
	parts := make([]string, 0, len(encoded)/2)
	for index := 0; index < len(encoded); index += 2 {
		parts = append(parts, encoded[index:index+2])
	}
	return strings.Join(parts, ":")
}

func stringsTrimFinalDot(value string) string {
	if len(value) > 0 && value[len(value)-1] == '.' {
		return value[:len(value)-1]
	}
	return value
}
