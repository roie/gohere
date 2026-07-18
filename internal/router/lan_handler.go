package router

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
)

type lanRouteBinding struct {
	ref         RouteRef
	certificate tls.Certificate
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
	s.lanRoutes[hostname] = lanRouteBinding{ref: ref, certificate: certificate}
	return nil
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
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if hello == nil || hello.ServerName == "" {
				return nil, errors.New("LAN TLS requires SNI")
			}
			hostname, err := normalizeLANHostname(hello.ServerName)
			if err != nil {
				return nil, err
			}
			s.lanMu.RLock()
			binding, ok := s.lanRoutes[hostname]
			s.lanMu.RUnlock()
			if !ok {
				return nil, fmt.Errorf("unknown LAN TLS hostname %s", hostname)
			}
			route, active, err := s.activeLANRoute(hostname)
			if err != nil {
				return nil, err
			}
			if !active || route.Ref() != binding.ref {
				return nil, fmt.Errorf("inactive LAN TLS hostname %s", hostname)
			}
			certificate := binding.certificate
			return &certificate, nil
		},
	}
}

func (s *Server) LANHTTPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
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
