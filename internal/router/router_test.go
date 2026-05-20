package router

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTokenGeneratedWith0600Permissions(t *testing.T) {
	dir := t.TempDir()

	token, err := EnsureToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) < 32 {
		t.Fatalf("token too short: %q", token)
	}

	info, err := os.Stat(filepath.Join(dir, "token"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("token permissions = %v, want 0600", got)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "token.*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary token files were left behind: %#v", matches)
	}
}

func TestReadTokenDoesNotCreateMissingTokenFile(t *testing.T) {
	dir := t.TempDir()

	_, err := ReadToken(dir)
	if err == nil {
		t.Fatal("expected missing token error")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "token")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("token file stat err = %v, want not exist", statErr)
	}
}

func TestReadTokenRejectsInvalidTokenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("short\n"), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := ReadToken(dir)
	if err == nil {
		t.Fatal("expected invalid token error")
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != "short\n" {
		t.Fatalf("token file = %q, want unchanged invalid token", string(data))
	}
}

func TestEnsureTokenRegeneratesEmptyTokenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("\n"), 0644); err != nil {
		t.Fatal(err)
	}

	token, err := EnsureToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) < 32 {
		t.Fatalf("token = %q, want regenerated token", token)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != token {
		t.Fatalf("token file = %q, returned token %q", string(data), token)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("token permissions = %v, want 0600", got)
	}
}

func TestEnsureTokenRegeneratesInvalidShortTokenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("short\n"), 0600); err != nil {
		t.Fatal(err)
	}

	token, err := EnsureToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) < 32 || token == "short" {
		t.Fatalf("token = %q, want regenerated token", token)
	}
}

func TestEnsureTokenRegeneratesNonHexTokenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	badToken := strings.Repeat("z", tokenLength)
	if err := os.WriteFile(path, []byte(badToken+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	token, err := EnsureToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if token == badToken || len(token) != tokenLength {
		t.Fatalf("token = %q, want regenerated %d-char hex token", token, tokenLength)
	}
}

func TestRouteStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := NewRouteStore(filepath.Join(dir, "routes.json"))
	route := Route{
		Host:      "eventca.localhost",
		Target:    "http://127.0.0.1:49231",
		PID:       12345,
		CWD:       "/home/roie/code/eventca",
		Name:      "eventca",
		Source:    "wsl",
		OwnerCWD:  "/home/roie/code/eventca",
		OwnerEnv:  "wsl",
		StartedAt: time.Date(2026, 5, 16, 0, 0, 0, 0, time.UTC),
	}

	if err := store.Save([]Route{route}); err != nil {
		t.Fatal(err)
	}
	routes, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0].Host != route.Host || routes[0].Target != route.Target || routes[0].Source != "wsl" || routes[0].OwnerCWD != route.OwnerCWD || routes[0].OwnerEnv != "wsl" {
		t.Fatalf("routes = %#v", routes)
	}
}

func TestAdminAPIRequiresTokenForRoutes(t *testing.T) {
	srv := NewServer(Config{Token: "secret", Store: NewMemoryStore()})

	req := httptest.NewRequest(http.MethodGet, "/routes", nil)
	rec := httptest.NewRecorder()
	srv.AdminHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /routes without token = %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/health", nil)
	rec = httptest.NewRecorder()
	srv.AdminHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /health = %d", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) != "gohere-router" {
		t.Fatalf("GET /health body = %q", rec.Body.String())
	}
}

func TestAdminAPIProbeTargetRequiresToken(t *testing.T) {
	srv := NewServer(Config{Token: "secret", Store: NewMemoryStore()})

	req := httptest.NewRequest(http.MethodPost, "/probe-target", strings.NewReader(`{"target":"http://127.0.0.1:1"}`))
	rec := httptest.NewRecorder()
	srv.AdminHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /probe-target without token = %d", rec.Code)
	}
}

func TestAdminAPIProbeTargetReachable(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	defer target.Close()
	srv := NewServer(Config{Token: "secret", Store: NewMemoryStore()})

	req := httptest.NewRequest(http.MethodPost, "/probe-target", strings.NewReader(`{"target":`+fmt.Sprintf("%q", target.URL)+`}`))
	req.Header.Set("X-Gohere-Token", "secret")
	rec := httptest.NewRecorder()
	srv.AdminHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /probe-target = %d body %q", rec.Code, rec.Body.String())
	}
	var response struct {
		Reachable bool `json:"reachable"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Reachable {
		t.Fatalf("probe response = %#v", response)
	}
}

func TestAdminAPIProbeTargetUnreachable(t *testing.T) {
	srv := NewServer(Config{Token: "secret", Store: NewMemoryStore()})

	req := httptest.NewRequest(http.MethodPost, "/probe-target", strings.NewReader(`{"target":"http://127.0.0.1:1"}`))
	req.Header.Set("X-Gohere-Token", "secret")
	rec := httptest.NewRecorder()
	srv.AdminHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /probe-target = %d body %q", rec.Code, rec.Body.String())
	}
	var response struct {
		Reachable bool `json:"reachable"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Reachable {
		t.Fatalf("probe response = %#v", response)
	}
}

func TestAdminAPIProbeTargetRejectsNonHTTPURL(t *testing.T) {
	srv := NewServer(Config{Token: "secret", Store: NewMemoryStore()})

	req := httptest.NewRequest(http.MethodPost, "/probe-target", strings.NewReader(`{"target":"file:///etc/passwd"}`))
	req.Header.Set("X-Gohere-Token", "secret")
	rec := httptest.NewRecorder()
	srv.AdminHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /probe-target = %d body %q", rec.Code, rec.Body.String())
	}
}

func TestAdminAPIRejectsRoutesWhenTokenIsEmpty(t *testing.T) {
	srv := NewServer(Config{Token: "", Store: NewMemoryStore()})

	req := httptest.NewRequest(http.MethodGet, "/routes", nil)
	rec := httptest.NewRecorder()
	srv.AdminHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /routes with empty configured token = %d", rec.Code)
	}
}

func TestAdminAPIRouteCRUD(t *testing.T) {
	store := NewMemoryStore()
	srv := NewServer(Config{Token: "secret", Store: store})
	handler := srv.AdminHandler()

	body := strings.NewReader(`{"host":"eventca.localhost","target":"http://127.0.0.1:49231","pid":12345,"cwd":"/tmp/eventca","name":"eventca","source":"wsl","ownerCwd":"/home/roie/project","ownerEnv":"wsl","startedAt":"2026-05-16T00:00:00Z"}`)
	req := httptest.NewRequest(http.MethodPost, "/routes", body)
	req.Header.Set("X-Gohere-Token", "secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST /routes = %d body %q", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/routes", nil)
	req.Header.Set("X-Gohere-Token", "secret")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /routes = %d", rec.Code)
	}
	var routes []Route
	if err := json.Unmarshal(rec.Body.Bytes(), &routes); err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0].Host != "eventca.localhost" || routes[0].Source != "wsl" || routes[0].OwnerCWD != "/home/roie/project" || routes[0].OwnerEnv != "wsl" {
		t.Fatalf("routes = %#v", routes)
	}

	req = httptest.NewRequest(http.MethodDelete, "/routes/eventca.localhost", nil)
	req.Header.Set("X-Gohere-Token", "secret")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE /routes/host = %d", rec.Code)
	}
	if routes, _ := store.Load(); len(routes) != 0 {
		t.Fatalf("routes after delete = %#v", routes)
	}
}

func TestAdminAPIUpsertHostCaseInsensitive(t *testing.T) {
	store := NewMemoryStore()
	store.Save([]Route{{Host: "eventca.localhost", Target: "http://127.0.0.1:49231"}})
	srv := NewServer(Config{Token: "secret", Store: store})

	body := strings.NewReader(`{"host":"EventCA.localhost","target":"http://127.0.0.1:49232"}`)
	req := httptest.NewRequest(http.MethodPost, "/routes", body)
	req.Header.Set("X-Gohere-Token", "secret")
	rec := httptest.NewRecorder()
	srv.AdminHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST /routes = %d body %q", rec.Code, rec.Body.String())
	}
	routes, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0].Target != "http://127.0.0.1:49232" {
		t.Fatalf("routes = %#v", routes)
	}
}

func TestAdminAPIDeleteHostCaseInsensitive(t *testing.T) {
	store := NewMemoryStore()
	store.Save([]Route{{Host: "eventca.localhost", Target: "http://127.0.0.1:49231"}})
	srv := NewServer(Config{Token: "secret", Store: store})

	req := httptest.NewRequest(http.MethodDelete, "/routes/EventCA.localhost", nil)
	req.Header.Set("X-Gohere-Token", "secret")
	rec := httptest.NewRecorder()
	srv.AdminHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE /routes/host = %d", rec.Code)
	}
	if routes, _ := store.Load(); len(routes) != 0 {
		t.Fatalf("routes after delete = %#v", routes)
	}
}

func TestProxyRoutesByHostHeader(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend-Host", r.Host)
		io.WriteString(w, "proxied response")
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Save([]Route{{Host: "eventca.localhost", Target: backend.URL}})
	srv := NewServer(Config{Token: "secret", Store: store})

	req := httptest.NewRequest(http.MethodGet, "http://eventca.localhost/", nil)
	req.Host = "eventca.localhost"
	rec := httptest.NewRecorder()
	srv.HTTPHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("proxy status = %d body %q", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "proxied response" {
		t.Fatalf("proxy body = %q", rec.Body.String())
	}
	if rec.Header().Get("X-Backend-Host") != "eventca.localhost" {
		t.Fatalf("backend Host = %q, want eventca.localhost", rec.Header().Get("X-Backend-Host"))
	}
}

func TestProxyHostMatchIsCaseInsensitive(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "proxied response")
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Save([]Route{{Host: "eventca.localhost", Target: backend.URL}})
	srv := NewServer(Config{Token: "secret", Store: store})

	req := httptest.NewRequest(http.MethodGet, "http://EventCA.localhost/", nil)
	req.Host = "EventCA.localhost"
	rec := httptest.NewRecorder()
	srv.HTTPHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("proxy status = %d body %q", rec.Code, rec.Body.String())
	}
}

func TestProxySupportsUpgradeRequests(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			t.Errorf("Upgrade header = %q", r.Header.Get("Upgrade"))
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("response writer is not hijackable")
			return
		}
		conn, bufrw, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		bufrw.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
		bufrw.WriteString("Upgrade: websocket\r\n")
		bufrw.WriteString("Connection: Upgrade\r\n\r\n")
		bufrw.Flush()
		message, err := bufrw.ReadString('\n')
		if err != nil {
			t.Errorf("backend read upgraded stream: %v", err)
			return
		}
		bufrw.WriteString("backend:" + message)
		bufrw.Flush()
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Save([]Route{{Host: "hmr.localhost", Target: backend.URL}})
	routerServer := httptest.NewServer(NewServer(Config{Token: "secret", Store: store}).HTTPHandler())
	defer routerServer.Close()

	addr := strings.TrimPrefix(routerServer.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	io.WriteString(conn, "GET /hmr HTTP/1.1\r\n")
	io.WriteString(conn, "Host: hmr.localhost\r\n")
	io.WriteString(conn, "Connection: Upgrade\r\n")
	io.WriteString(conn, "Upgrade: websocket\r\n")
	io.WriteString(conn, "Sec-WebSocket-Version: 13\r\n")
	io.WriteString(conn, "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade status = %d", resp.StatusCode)
	}
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	echo, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if echo != "backend:ping\n" {
		t.Fatalf("upgrade echo = %q", echo)
	}
}

func TestMissingRouteShowsFriendlyPage(t *testing.T) {
	srv := NewServer(Config{Token: "secret", Store: NewMemoryStore()})
	req := httptest.NewRequest(http.MethodGet, "http://missing.localhost/", nil)
	req.Host = "missing.localhost"
	rec := httptest.NewRecorder()

	srv.HTTPHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("missing route status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "No gohere route is running for missing.localhost") {
		t.Fatalf("missing route body = %q", rec.Body.String())
	}
}

func TestListenAndProxyOnHighPort(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "from backend")
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Save([]Route{{Host: "app.localhost", Target: backend.URL}})
	srv := NewServer(Config{Token: "secret", Store: store})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go http.Serve(ln, srv.HTTPHandler())

	req, err := http.NewRequest(http.MethodGet, "http://"+ln.Addr().String()+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "app.localhost"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)

	if string(data) != "from backend" {
		t.Fatalf("proxy response = %q", string(data))
	}
}

func TestRotateLogKeepsOneBackup(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "router.log")
	if err := os.WriteFile(logPath, bytes.Repeat([]byte("x"), maxLogSize+1), 0644); err != nil {
		t.Fatal(err)
	}

	if err := RotateLog(logPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logPath + ".1"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatalf("new log size = %d, want 0", info.Size())
	}
}

func TestStartRunsAdminHealthAndCreatesState(t *testing.T) {
	ctx := t.Context()
	stateDir := t.TempDir()

	running, err := Start(ctx, StartConfig{
		HTTPAddr:  "127.0.0.1:0",
		AdminAddr: "127.0.0.1:0",
		StateDir:  stateDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer running.Close()

	resp, err := http.Get("http://" + running.AdminAddr + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "token")); err != nil {
		t.Fatal(err)
	}
}

func TestStartWritesRouterPID(t *testing.T) {
	stateDir := t.TempDir()

	running, err := Start(t.Context(), StartConfig{
		HTTPAddr:  "127.0.0.1:0",
		AdminAddr: "127.0.0.1:0",
		StateDir:  stateDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer running.Close()

	data, err := os.ReadFile(filepath.Join(stateDir, "router.pid"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) == "" {
		t.Fatalf("router.pid is empty")
	}
}

func TestCloseRemovesRouterPID(t *testing.T) {
	stateDir := t.TempDir()
	running, err := Start(t.Context(), StartConfig{
		HTTPAddr:  "127.0.0.1:0",
		AdminAddr: "127.0.0.1:0",
		StateDir:  stateDir,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := running.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "router.pid")); !os.IsNotExist(err) {
		t.Fatalf("router.pid should be removed, stat err = %v", err)
	}
}

func TestCloseAllowsInFlightRequestToFinish(t *testing.T) {
	stateDir := t.TempDir()
	store := NewRouteStore(filepath.Join(stateDir, "routes.json"))
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("done"))
	}))
	defer backend.Close()
	if err := store.Save([]Route{{Host: "app.localhost", Target: backend.URL}}); err != nil {
		t.Fatal(err)
	}
	running, err := Start(t.Context(), StartConfig{
		HTTPAddr:  "127.0.0.1:0",
		AdminAddr: "127.0.0.1:0",
		StateDir:  stateDir,
	})
	if err != nil {
		t.Fatal(err)
	}

	errc := make(chan error, 1)
	go func() {
		req, err := http.NewRequest(http.MethodGet, "http://"+running.HTTPAddr+"/", nil)
		if err != nil {
			errc <- err
			return
		}
		req.Host = "app.localhost"
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			errc <- err
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			errc <- fmt.Errorf("status = %d", resp.StatusCode)
			return
		}
		errc <- nil
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not reach backend")
	}
	closeDone := make(chan error, 1)
	go func() {
		closeDone <- running.Close()
	}()
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned before in-flight request completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight request did not finish")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not finish after in-flight request completed")
	}
	if _, err := os.Stat(filepath.Join(stateDir, "router.pid")); !os.IsNotExist(err) {
		t.Fatalf("router.pid should be removed, stat err = %v", err)
	}
}

func TestStartDoesNotLeavePIDWhenListenFails(t *testing.T) {
	stateDir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	_, err = Start(t.Context(), StartConfig{
		HTTPAddr:  ln.Addr().String(),
		AdminAddr: "127.0.0.1:0",
		StateDir:  stateDir,
	})
	if err == nil {
		t.Fatal("expected listen failure")
	}
	if _, err := os.Stat(filepath.Join(stateDir, "router.pid")); !os.IsNotExist(err) {
		t.Fatalf("router.pid should not exist after failed start, stat err = %v", err)
	}
}

func TestStartReportsClearPortConflict(t *testing.T) {
	stateDir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	_, err = Start(t.Context(), StartConfig{
		HTTPAddr:  ln.Addr().String(),
		AdminAddr: "127.0.0.1:0",
		StateDir:  stateDir,
	})
	if err == nil {
		t.Fatal("expected listen failure")
	}
	msg := err.Error()
	if !strings.Contains(msg, "gohere router cannot listen on") || !strings.Contains(msg, "port is already in use") {
		t.Fatalf("listen error = %q", msg)
	}
}

func TestListenErrorIncludesPortOwnerWhenDetected(t *testing.T) {
	old := findPortOwner
	findPortOwner = func(port string) string {
		if port != "80" {
			t.Fatalf("port lookup = %q, want 80", port)
		}
		return "nginx 1234"
	}
	defer func() { findPortOwner = old }()

	err := listenError("127.0.0.1:80", fmt.Errorf("listen tcp 127.0.0.1:80: bind: address already in use"))
	msg := err.Error()
	if !strings.Contains(msg, "port is already in use") || !strings.Contains(msg, "owning process: nginx 1234") {
		t.Fatalf("listen error = %q", msg)
	}
}

func TestStartRotatesDefaultRouterLog(t *testing.T) {
	ctx := t.Context()
	stateDir := t.TempDir()
	logPath := filepath.Join(stateDir, "logs", "router.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, bytes.Repeat([]byte("x"), maxLogSize+1), 0644); err != nil {
		t.Fatal(err)
	}

	running, err := Start(ctx, StartConfig{
		HTTPAddr:  "127.0.0.1:0",
		AdminAddr: "127.0.0.1:0",
		StateDir:  stateDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer running.Close()

	if _, err := os.Stat(logPath + ".1"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 || info.Size() > maxLogSize {
		t.Fatalf("router.log size = %d, want non-empty capped log", info.Size())
	}
}

func TestStartWritesRouterLog(t *testing.T) {
	stateDir := t.TempDir()
	logPath := filepath.Join(stateDir, "logs", "router.log")

	running, err := Start(t.Context(), StartConfig{
		HTTPAddr:  "127.0.0.1:0",
		AdminAddr: "127.0.0.1:0",
		StateDir:  stateDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer running.Close()

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "gohere router started") {
		t.Fatalf("router log = %q", string(data))
	}
}

func TestStartRejectsNonLoopbackAdminAddress(t *testing.T) {
	_, err := Start(t.Context(), StartConfig{
		HTTPAddr:  "127.0.0.1:0",
		AdminAddr: "0.0.0.0:0",
		StateDir:  t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected non-loopback admin address to be rejected")
	}
}
