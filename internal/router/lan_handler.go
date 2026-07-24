package router

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

type lanRouteBinding struct {
	ref           RouteRef
	certificate   tls.Certificate
	caFingerprint string
}

type lanTLSAuthorization struct {
	hostname      string
	ref           RouteRef
	caFingerprint string
}

func (s *Server) RegisterLANRoute(ref RouteRef, hostname string, certificate tls.Certificate) error {
	hostname, err := normalizeLANHostname(hostname)
	if err != nil {
		return err
	}
	if ref.ID == "" || ref.Generation == 0 {
		return errors.New("LAN route identity is required")
	}
	s.lanMu.Lock()
	defer s.lanMu.Unlock()
	if existing, ok := s.lanRoutes[hostname]; ok && existing.ref != ref {
		return fmt.Errorf("LAN hostname %s is already registered", hostname)
	}
	s.lanRoutes[hostname] = lanRouteBinding{
		ref: ref, certificate: certificate,
		caFingerprint: lanCertificateAuthorityFingerprint(certificate),
	}
	return nil
}

func (s *Server) replaceLANCertificate(ref RouteRef, hostname string, certificate tls.Certificate) bool {
	hostname, err := normalizeLANHostname(hostname)
	if err != nil {
		return false
	}
	s.lanMu.Lock()
	defer s.lanMu.Unlock()
	binding, ok := s.lanRoutes[hostname]
	if !ok || binding.ref != ref {
		return false
	}
	binding.certificate = certificate
	binding.caFingerprint = lanCertificateAuthorityFingerprint(certificate)
	s.lanRoutes[hostname] = binding
	return true
}

func (s *Server) RemoveLANRoute(ref RouteRef) {
	s.lanMu.Lock()
	defer s.lanMu.Unlock()
	for hostname, binding := range s.lanRoutes {
		if binding.ref == ref {
			delete(s.lanRoutes, hostname)
		}
	}
}

func (s *Server) RemoveLANHostname(ref RouteRef, hostname string) {
	hostname, err := normalizeLANHostname(hostname)
	if err != nil {
		return
	}
	s.lanMu.Lock()
	defer s.lanMu.Unlock()
	if binding, ok := s.lanRoutes[hostname]; ok && binding.ref == ref {
		delete(s.lanRoutes, hostname)
	}
}

func (s *Server) LANTLSConfig() *tls.Config {
	configured := &tls.Config{
		MinVersion:             tls.VersionTLS12,
		SessionTicketsDisabled: true,
	}
	configured.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		if hello == nil {
			return nil, errors.New("LAN TLS ClientHello is required")
		}
		authorization, err := s.authorizeLANTLS(hello.ServerName)
		if err != nil {
			return nil, err
		}

		selected := configured.Clone()
		selected.GetConfigForClient = nil
		selected.Certificates = nil
		selected.NameToCertificate = nil
		selected.SessionTicketsDisabled = true
		selected.GetCertificate = func(current *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if current == nil {
				return nil, errors.New("LAN TLS ClientHello is required")
			}
			hostname, err := normalizeLANHostname(current.ServerName)
			if err != nil {
				return nil, err
			}
			if hostname != authorization.hostname {
				return nil, errors.New("LAN TLS server name no longer matches authorization")
			}
			return s.revalidateLANTLS(authorization)
		}
		selected.VerifyConnection = func(state tls.ConnectionState) error {
			hostname, err := normalizeLANHostname(state.ServerName)
			if err != nil || hostname != authorization.hostname {
				return errors.New("LAN TLS server name no longer matches authorization")
			}
			_, err = s.revalidateLANTLS(authorization)
			return err
		}
		return selected, nil
	}
	return configured
}

func (s *Server) authorizeLANTLS(rawHostname string) (lanTLSAuthorization, error) {
	if rawHostname == "" {
		return lanTLSAuthorization{}, errors.New("LAN TLS requires SNI")
	}
	hostname, err := normalizeLANHostname(rawHostname)
	if err != nil {
		return lanTLSAuthorization{}, err
	}
	s.lanMu.RLock()
	binding, ok := s.lanRoutes[hostname]
	s.lanMu.RUnlock()
	if !ok {
		return lanTLSAuthorization{}, fmt.Errorf("unknown LAN TLS hostname %s", hostname)
	}
	authorization := lanTLSAuthorization{
		hostname: hostname, ref: binding.ref, caFingerprint: binding.caFingerprint,
	}
	if _, err := s.revalidateLANTLS(authorization); err != nil {
		return lanTLSAuthorization{}, err
	}
	return authorization, nil
}

func (s *Server) revalidateLANTLS(authorization lanTLSAuthorization) (*tls.Certificate, error) {
	s.lanMu.RLock()
	binding, ok := s.lanRoutes[authorization.hostname]
	currentCAFingerprint := s.currentLANCAFingerprint
	s.lanMu.RUnlock()
	if !ok || binding.ref != authorization.ref || binding.caFingerprint != authorization.caFingerprint {
		return nil, fmt.Errorf("LAN TLS authorization changed for %s", authorization.hostname)
	}
	if currentCAFingerprint != nil {
		current, err := currentCAFingerprint()
		if err != nil {
			return nil, fmt.Errorf("load current LAN certificate authority: %w", err)
		}
		if authorization.caFingerprint == "" || authorization.caFingerprint != current {
			return nil, fmt.Errorf("stale LAN TLS certificate for %s", authorization.hostname)
		}
	}
	route, active, err := s.activeLANRoute(authorization.hostname)
	if err != nil {
		return nil, err
	}
	if !active || route.Ref() != authorization.ref {
		return nil, fmt.Errorf("inactive LAN TLS hostname %s", authorization.hostname)
	}

	s.lanMu.RLock()
	binding, ok = s.lanRoutes[authorization.hostname]
	if !ok || binding.ref != authorization.ref || binding.caFingerprint != authorization.caFingerprint {
		s.lanMu.RUnlock()
		return nil, fmt.Errorf("LAN TLS authorization changed for %s", authorization.hostname)
	}
	certificate := binding.certificate
	s.lanMu.RUnlock()
	return &certificate, nil
}

func lanCertificateAuthorityFingerprint(certificate tls.Certificate) string {
	if len(certificate.Certificate) < 2 {
		return ""
	}
	return lanCAFingerprint(certificate.Certificate[len(certificate.Certificate)-1])
}

func lanCAFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

func (s *Server) LANHTTPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if s.serveLANTrust(w, request) {
			return
		}
		hostname, err := normalizeLANHostname(request.Host)
		if err != nil {
			http.NotFound(w, request)
			return
		}
		if _, ok, err := s.activeLANRoute(hostname); err != nil {
			http.Error(w, "LAN route unavailable", http.StatusServiceUnavailable)
			return
		} else if !ok {
			http.NotFound(w, request)
			return
		}
		location := "https://" + strings.TrimSuffix(hostname, ".") + request.URL.RequestURI()
		http.Redirect(w, request, location, http.StatusPermanentRedirect)
	})
}

func (s *Server) LANHandler() http.Handler {
	loopbackHandler := s.HTTPHandler()
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == RouterIdentityPath || request.TLS == nil || request.TLS.ServerName == "" {
			http.Error(w, "LAN request requires matching Host and SNI", http.StatusMisdirectedRequest)
			return
		}
		hostname, hostErr := normalizeLANHostname(request.Host)
		serverName, sniErr := normalizeLANHostname(request.TLS.ServerName)
		if hostErr != nil || sniErr != nil {
			http.NotFound(w, request)
			return
		}
		if hostname != serverName {
			http.Error(w, "LAN Host and SNI do not match", http.StatusMisdirectedRequest)
			return
		}
		route, ok, err := s.activeLANRoute(hostname)
		if err != nil {
			http.Error(w, "LAN route unavailable", http.StatusServiceUnavailable)
			return
		}
		if !ok {
			http.NotFound(w, request)
			return
		}
		s.storeMu.Lock()
		_ = MarkLANShareConnected(s.store, route.Ref(), time.Now())
		s.storeMu.Unlock()
		forwarded := request.Clone(withLANProxyContext(request.Context(), hostname, route.Host))
		forwarded.Host = route.Host
		loopbackHandler.ServeHTTP(w, forwarded)
	})
}

func (s *Server) activeLANRoute(hostname string) (Route, bool, error) {
	s.lanMu.RLock()
	binding, ok := s.lanRoutes[hostname]
	s.lanMu.RUnlock()
	if !ok {
		return Route{}, false, nil
	}
	routes, err := s.loadRoutes()
	if err != nil {
		return Route{}, false, err
	}
	index := routeRefIndex(routes, binding.ref)
	if index < 0 || !activeLANShare(routes[index], hostname) {
		return Route{}, false, nil
	}
	return routes[index], true, nil
}

func activeLANShare(route Route, hostname string) bool {
	if route.LANShare == nil || route.LANShare.State != LANShareActive {
		return false
	}
	current, err := normalizeLANHostname(route.LANShare.Hostname)
	return err == nil && current == hostname
}

func normalizeLANHostname(host string) (string, error) {
	host = strings.TrimSpace(strings.ToLower(host))
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	}
	host = strings.TrimSuffix(host, ".")
	label, ok := strings.CutSuffix(host, ".local")
	if !ok || label == "" || len(label) > 63 || strings.Contains(label, ".") || label[0] == '-' || label[len(label)-1] == '-' {
		return "", fmt.Errorf("invalid LAN hostname %q", host)
	}
	for _, character := range label {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return "", fmt.Errorf("invalid LAN hostname %q", host)
		}
	}
	return label + ".local.", nil
}
