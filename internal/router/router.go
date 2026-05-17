package router

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const maxLogSize = 1024 * 1024

type Route struct {
	Host      string    `json:"host"`
	Target    string    `json:"target"`
	PID       int       `json:"pid"`
	CWD       string    `json:"cwd"`
	Name      string    `json:"name"`
	StartedAt time.Time `json:"startedAt"`
}

type Store interface {
	Load() ([]Route, error)
	Save([]Route) error
}

type Config struct {
	Token string
	Store Store
}

type Server struct {
	token string
	store Store
}

func NewServer(cfg Config) *Server {
	return &Server{token: cfg.Token, store: cfg.Store}
}

func EnsureToken(stateDir string) (string, error) {
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return "", err
	}
	path := filepath.Join(stateDir, "token")
	data, err := os.ReadFile(path)
	if err == nil {
		if err := os.Chmod(path, 0600); err != nil {
			return "", err
		}
		return strings.TrimSpace(string(data)), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", err
	}
	token := hex.EncodeToString(tokenBytes)
	if err := os.WriteFile(path, []byte(token+"\n"), 0600); err != nil {
		return "", err
	}
	return token, nil
}

type RouteStore struct {
	path string
}

func NewRouteStore(path string) *RouteStore {
	return &RouteStore{path: path}
}

func (s *RouteStore) Load() ([]Route, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var routes []Route
	if err := json.Unmarshal(data, &routes); err != nil {
		return nil, err
	}
	sortRoutes(routes)
	return routes, nil
}

func (s *RouteStore) Save(routes []Route) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	sortRoutes(routes)
	data, err := json.MarshalIndent(routes, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(s.path, data, 0600)
}

type MemoryStore struct {
	mu     sync.Mutex
	routes []Route
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{}
}

func (s *MemoryStore) Load() ([]Route, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	routes := append([]Route(nil), s.routes...)
	sortRoutes(routes)
	return routes, nil
}

func (s *MemoryStore) Save(routes []Route) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.routes = append([]Route(nil), routes...)
	sortRoutes(s.routes)
	return nil
}

func (s *Server) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/routes", s.handleRoutes)
	mux.HandleFunc("/routes/", s.handleRoute)
	return mux
}

func (s *Server) HTTPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route, ok, err := s.routeForHost(r.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			missingRoutePage(w, r.Host)
			return
		}

		target, err := url.Parse(route.Target)
		if err != nil {
			http.Error(w, "invalid gohere route target", http.StatusBadGateway)
			return
		}
		proxy := httputil.NewSingleHostReverseProxy(target)
		originalDirector := proxy.Director
		proxy.Director = func(req *http.Request) {
			originalDirector(req)
			req.Host = target.Host
		}
		proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			missingRoutePage(w, r.Host)
		}
		proxy.ServeHTTP(w, r)
	})
}

func (s *Server) handleRoutes(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	switch r.Method {
	case http.MethodGet:
		routes, err := s.store.Load()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(routes)
	case http.MethodPost:
		var route Route
		if err := json.NewDecoder(r.Body).Decode(&route); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if route.Host == "" || route.Target == "" {
			http.Error(w, "host and target are required", http.StatusBadRequest)
			return
		}
		routes, err := s.store.Load()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		routes = upsertRoute(routes, route)
		if err := s.store.Save(routes); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleRoute(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	host := strings.ToLower(strings.TrimPrefix(r.URL.Path, "/routes/"))
	routes, err := s.store.Load()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	filtered := routes[:0]
	for _, route := range routes {
		if strings.ToLower(route.Host) != host {
			filtered = append(filtered, route)
		}
	}
	if err := s.store.Save(filtered); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) authorized(r *http.Request) bool {
	return s.token != "" && r.Header.Get("X-Gohere-Token") == s.token
}

func (s *Server) routeForHost(host string) (Route, bool, error) {
	if before, _, ok := strings.Cut(host, ":"); ok {
		host = before
	}
	host = strings.ToLower(host)
	routes, err := s.store.Load()
	if err != nil {
		return Route{}, false, err
	}
	for _, route := range routes {
		if strings.ToLower(route.Host) == host {
			return route, true, nil
		}
	}
	return Route{}, false, nil
}

func RotateLog(logPath string) error {
	info, err := os.Stat(logPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if info.Size() <= maxLogSize {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0700); err != nil {
		return err
	}
	os.Remove(logPath + ".1")
	if err := os.Rename(logPath, logPath+".1"); err != nil {
		return err
	}
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	return file.Close()
}

func missingRoutePage(w http.ResponseWriter, host string) {
	if before, _, ok := strings.Cut(host, ":"); ok {
		host = before
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadGateway)
	io.WriteString(w, "<!doctype html><title>gohere route missing</title><h1>No gohere route is running for "+html.EscapeString(host)+"</h1>")
}

func upsertRoute(routes []Route, route Route) []Route {
	for i := range routes {
		if routes[i].Host == route.Host {
			routes[i] = route
			return routes
		}
	}
	return append(routes, route)
}

func sortRoutes(routes []Route) {
	sort.Slice(routes, func(i, j int) bool {
		return routes[i].Host < routes[j].Host
	})
}
