package router

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	goherecert "github.com/roie/gohere/internal/cert"
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

type lanManagerConfig struct {
	store            Store
	selectInterface  func(context.Context) (laninterface.Candidate, error)
	issueCertificate func(string, time.Time) (tls.Certificate, error)
	startIngress     func(context.Context, *Server, string, string) (lanIngress, error)
	newResponder     func(context.Context, lanmdns.Interface, lanmdns.Coordinator) (lanHostnameResponder, error)
	now              func() time.Time
}

type LANShareResult struct {
	Hostname string     `json:"hostname"`
	URL      string     `json:"url"`
	Address  netip.Addr `json:"address"`
}

type LANManager struct {
	ctx    context.Context
	server *Server
	config lanManagerConfig

	mu               sync.Mutex
	iface            laninterface.Candidate
	ingress          lanIngress
	responder        lanHostnameResponder
	pending          map[string]RouteRef
	registrations    map[RouteRef]lanmdns.Registration
	registrationRefs map[lanmdns.RegistrationID]RouteRef
	closeOnce        sync.Once
	closeErr         error
}

func NewLANManager(ctx context.Context, server *Server, stateDir string) *LANManager {
	certificateStore := goherecert.Store{StateDir: stateDir}
	return newLANManager(ctx, server, lanManagerConfig{
		store: server.store,
		selectInterface: func(ctx context.Context) (laninterface.Candidate, error) {
			return laninterface.DiscoverAndSelect(ctx, nil)
		},
		issueCertificate: certificateStore.IssueEphemeralLANHostCert,
		startIngress: func(ctx context.Context, server *Server, httpAddr, httpsAddr string) (lanIngress, error) {
			return StartLANIngress(ctx, server, httpAddr, httpsAddr)
		},
		newResponder: func(ctx context.Context, iface lanmdns.Interface, coordinator lanmdns.Coordinator) (lanHostnameResponder, error) {
			return lanmdns.New(ctx, iface, coordinator)
		},
		now: time.Now,
	})
}

func newLANManager(ctx context.Context, server *Server, config lanManagerConfig) *LANManager {
	if ctx == nil {
		ctx = context.Background()
	}
	return &LANManager{
		ctx: ctx, server: server, config: config,
		pending: make(map[string]RouteRef), registrations: make(map[RouteRef]lanmdns.Registration),
		registrationRefs: make(map[lanmdns.RegistrationID]RouteRef),
	}
}

func (m *LANManager) Create(ctx context.Context, ref RouteRef) (LANShareResult, error) {
	m.mu.Lock()
	if registration, ok := m.registrations[ref]; ok {
		hostname := registration.CurrentHostname()
		m.mu.Unlock()
		return m.resultFor(hostname), nil
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
	return m.resultFor(registration.CurrentHostname()), nil
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
	if err := m.server.RegisterLANRoute(ref, change.Proposed, certificate); err != nil {
		return err
	}
	activation := LANActivation{
		Hostname: change.Proposed, InterfaceIndex: iface.Index, InterfaceName: iface.Name,
		Address: iface.Prefix.Addr().String(), Prefix: iface.Prefix.String(),
	}
	if err := ActivateLANShare(m.config.store, ref, activation); err != nil {
		m.server.RemoveLANHostname(ref, change.Proposed)
		return err
	}
	if change.Previous != "" && change.Previous != change.Proposed {
		m.server.RemoveLANHostname(ref, change.Previous)
	}
	return nil
}

func (m *LANManager) Remove(ctx context.Context, ref RouteRef) error {
	m.mu.Lock()
	registration := m.registrations[ref]
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
		m.mu.Lock()
		registrations := m.registrations
		m.registrations = make(map[RouteRef]lanmdns.Registration)
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
	return nil
}

func (m *LANManager) rollbackCreate(ref RouteRef, requested string) {
	m.server.RemoveLANRoute(ref)
	_ = RemoveLANShare(m.config.store, ref)
	m.mu.Lock()
	delete(m.pending, requested)
	if len(m.registrations) == 0 {
		responder, ingress := m.responder, m.ingress
		m.responder, m.ingress = nil, nil
		m.iface = laninterface.Candidate{}
		m.mu.Unlock()
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

func (m *LANManager) resultFor(hostname string) LANShareResult {
	m.mu.Lock()
	address := m.iface.Prefix.Addr()
	m.mu.Unlock()
	return LANShareResult{
		Hostname: hostname, URL: "https://" + stringsTrimFinalDot(hostname), Address: address,
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

func stringsTrimFinalDot(value string) string {
	if len(value) > 0 && value[len(value)-1] == '.' {
		return value[:len(value)-1]
	}
	return value
}
