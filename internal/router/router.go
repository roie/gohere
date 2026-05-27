package router

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/roie/gohere/internal/probe"
)

const maxLogSize = 1024 * 1024
const tokenLength = 64
const maxAdminBodyBytes = 1024 * 1024
const RoutesFilename = "routes.json"

var rotateOpenFile = os.OpenFile

type Route struct {
	Host            string    `json:"host"`
	Target          string    `json:"target"`
	PID             int       `json:"pid"`
	CWD             string    `json:"cwd"`
	Name            string    `json:"name"`
	ProjectRoot     string    `json:"projectRoot,omitempty"`
	ProjectName     string    `json:"projectName,omitempty"`
	Source          string    `json:"source,omitempty"`
	OwnerCWD        string    `json:"ownerCwd,omitempty"`
	OwnerEnv        string    `json:"ownerEnv,omitempty"`
	StartedAt       time.Time `json:"startedAt"`
	ProcessIdentity string    `json:"processIdentity,omitempty"`
}

type RouteStatus struct {
	Route  Route  `json:"route"`
	Status string `json:"status"`
}

type Store interface {
	Load() ([]Route, error)
	Save([]Route) error
}

type Config struct {
	Token    string
	Store    Store
	Shutdown func()
}

type Server struct {
	token    string
	store    Store
	shutdown func()
	storeMu  sync.RWMutex
}

func NewServer(cfg Config) *Server {
	return &Server{token: cfg.Token, store: cfg.Store, shutdown: cfg.Shutdown}
}

func EnsureToken(stateDir string) (string, error) {
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return "", err
	}
	path := filepath.Join(stateDir, "token")
	data, err := os.ReadFile(path)
	if err == nil {
		token := strings.TrimSpace(string(data))
		if !validToken(token) {
			return writeToken(path)
		}
		if err := os.Chmod(path, 0600); err != nil {
			return "", err
		}
		return token, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	return writeToken(path)
}

func ReadToken(stateDir string) (string, error) {
	path := filepath.Join(stateDir, "token")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(data))
	if !validToken(token) {
		return "", fmt.Errorf("invalid gohere token")
	}
	if err := os.Chmod(path, 0600); err != nil {
		return "", err
	}
	return token, nil
}

func writeToken(path string) (string, error) {
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", err
	}
	token := hex.EncodeToString(tokenBytes)
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write([]byte(token + "\n")); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := replaceFile(tmpPath, path); err != nil {
		return "", err
	}
	if err := os.Chmod(path, 0600); err != nil {
		return "", err
	}
	cleanup = false
	return token, nil
}

func replaceFile(tmpPath, path string) error {
	return replaceFileForGOOS(runtime.GOOS, tmpPath, path)
}

func replaceFileForGOOS(goos, tmpPath, path string) error {
	return replaceFileForGOOSWithRename(goos, tmpPath, path, os.Rename)
}

func replaceFileForGOOSWithRename(goos, tmpPath, path string, rename func(string, string) error) error {
	if goos == "windows" {
		backup, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.bak")
		if err != nil {
			return err
		}
		backupPath := backup.Name()
		if err := backup.Close(); err != nil {
			os.Remove(backupPath)
			return err
		}
		if err := os.Remove(backupPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}

		hasExisting := true
		if err := rename(path, backupPath); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			hasExisting = false
		}
		if err := rename(tmpPath, path); err != nil {
			if hasExisting {
				_ = rename(backupPath, path)
			}
			return err
		}
		if hasExisting {
			_ = os.Remove(backupPath)
		}
		return nil
	}
	return rename(tmpPath, path)
}

func validToken(token string) bool {
	if len(token) != tokenLength {
		return false
	}
	for _, r := range token {
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'f' {
			continue
		}
		return false
	}
	return true
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
	tmp, err := os.CreateTemp(filepath.Dir(s.path), filepath.Base(s.path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := replaceFile(tmpPath, s.path); err != nil {
		return err
	}
	cleanup = false
	return os.Chmod(s.path, 0600)
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
		io.WriteString(w, "gohere-router\n")
	})
	mux.HandleFunc("/routes", s.handleRoutes)
	mux.HandleFunc("/route-statuses", s.handleRouteStatuses)
	mux.HandleFunc("/routes/", s.handleRoute)
	mux.HandleFunc("/probe-target", s.handleProbeTarget)
	mux.HandleFunc("/shutdown", s.handleShutdown)
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
			if isUpgradeRequest(r) {
				http.Error(w, "gohere websocket route missing", http.StatusBadGateway)
				return
			}
			missingRoutePage(w, r.Host)
			return
		}

		target, err := url.Parse(route.Target)
		if err != nil {
			http.Error(w, "invalid gohere route target", http.StatusBadGateway)
			return
		}
		proxy := httputil.NewSingleHostReverseProxy(target)
		proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			if isUpgradeRequest(r) {
				http.Error(w, "gohere websocket upstream unavailable", http.StatusBadGateway)
				return
			}
			missingRoutePage(w, r.Host)
		}
		proxy.ServeHTTP(w, r)
	})
}

func isUpgradeRequest(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

func (s *Server) handleRoutes(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	switch r.Method {
	case http.MethodGet:
		routes, err := s.loadRoutes()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(routes)
	case http.MethodPost:
		var route Route
		if err := json.NewDecoder(io.LimitReader(r.Body, maxAdminBodyBytes)).Decode(&route); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if route.Host == "" || route.Target == "" {
			http.Error(w, "host and target are required", http.StatusBadRequest)
			return
		}
		s.storeMu.Lock()
		defer s.storeMu.Unlock()
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

func (s *Server) handleRouteStatuses(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	routes, err := s.loadRoutes()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	statuses := make([]RouteStatus, 0, len(routes))
	for _, route := range routes {
		statuses = append(statuses, RouteStatus{
			Route:  route,
			Status: targetStatus(route.Target),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(statuses)
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

	rawHost := strings.TrimPrefix(r.URL.EscapedPath(), "/routes/")
	host, err := url.PathUnescape(rawHost)
	if err != nil {
		http.Error(w, "invalid route host", http.StatusBadRequest)
		return
	}
	host = strings.ToLower(host)
	s.storeMu.Lock()
	defer s.storeMu.Unlock()
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

func targetStatus(target string) string {
	return probe.TargetStatus(target)
}

func (s *Server) handleProbeTarget(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		Target string `json:"target"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxAdminBodyBytes)).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	target, err := url.Parse(body.Target)
	if err != nil || target.Scheme == "" || target.Host == "" {
		http.Error(w, "target must be an absolute URL", http.StatusBadRequest)
		return
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		http.Error(w, "target must use http or https", http.StatusBadRequest)
		return
	}
	if !allowedProbeTarget(target) {
		http.Error(w, "target must be local", http.StatusBadRequest)
		return
	}

	resp, err := probe.Head(target.String(), probe.DefaultTimeout)
	reachable := err == nil
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		Reachable bool `json:"reachable"`
	}{Reachable: reachable})
}

func allowedProbeTarget(target *url.URL) bool {
	host := target.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.WriteHeader(http.StatusNoContent)
	if s.shutdown != nil {
		go func() {
			defer func() {
				_ = recover()
			}()
			s.shutdown()
		}()
	}
}

func (s *Server) authorized(r *http.Request) bool {
	return s.token != "" && r.Header.Get("X-Gohere-Token") == s.token
}

func (s *Server) routeForHost(host string) (Route, bool, error) {
	host = hostWithoutPort(host)
	host = strings.ToLower(host)
	routes, err := s.loadRoutes()
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

func (s *Server) loadRoutes() ([]Route, error) {
	s.storeMu.RLock()
	defer s.storeMu.RUnlock()
	return s.store.Load()
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
	file, err := rotateOpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		if restoreErr := os.Rename(logPath+".1", logPath); restoreErr != nil {
			return errors.Join(err, fmt.Errorf("restore rotated log: %w", restoreErr))
		}
		return err
	}
	return file.Close()
}

func missingRoutePage(w http.ResponseWriter, host string) {
	host = hostWithoutPort(host)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadGateway)
	io.WriteString(w, "<!doctype html><title>gohere route missing</title><h1>No gohere route is running for "+html.EscapeString(host)+"</h1>")
}

func hostWithoutPort(host string) string {
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		return strings.Trim(parsed, "[]")
	}
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		return strings.Trim(host, "[]")
	}
	if strings.Count(host, ":") == 1 {
		before, _, _ := strings.Cut(host, ":")
		return before
	}
	return host
}

func upsertRoute(routes []Route, route Route) []Route {
	for i := range routes {
		if strings.EqualFold(routes[i].Host, route.Host) {
			routes[i] = route
			return routes
		}
	}
	return append(routes, route)
}

func sortRoutes(routes []Route) {
	sort.Slice(routes, func(i, j int) bool {
		left := strings.ToLower(routes[i].Host)
		right := strings.ToLower(routes[j].Host)
		if left == right {
			return routes[i].Host < routes[j].Host
		}
		return left < right
	})
}
