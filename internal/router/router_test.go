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
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestTokenGeneratedWith0600Permissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not preserve Unix permission bits")
	}
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
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not preserve Unix permission bits")
	}
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

func TestReplaceFileForWindowsReplacesExistingTarget(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "token")
	tmp := filepath.Join(dir, "token.tmp")
	if err := os.WriteFile(dst, []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("new\n"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := replaceFileForGOOS("windows", tmp, dst); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new\n" {
		t.Fatalf("target file = %q, want new contents", string(data))
	}
	if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp file stat err = %v, want not exist", err)
	}
}

func TestReplaceFileForWindowsRestoresExistingTargetWhenReplaceFails(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "token")
	tmp := filepath.Join(dir, "token.tmp")
	if err := os.WriteFile(dst, []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("new\n"), 0600); err != nil {
		t.Fatal(err)
	}

	errReplaceFailed := errors.New("replace failed")
	err := replaceFileForGOOSWithRename("windows", tmp, dst, func(oldPath, newPath string) error {
		if oldPath == tmp && newPath == dst {
			return errReplaceFailed
		}
		return os.Rename(oldPath, newPath)
	})
	if !errors.Is(err, errReplaceFailed) {
		t.Fatalf("err = %v, want replace failure", err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "old\n" {
		t.Fatalf("target file = %q, want old contents restored", string(data))
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

func TestSortRoutesCaseInsensitive(t *testing.T) {
	routes := []Route{
		{Host: "c.localhost"},
		{Host: "B.localhost"},
		{Host: "a.localhost"},
	}

	sortRoutes(routes)

	got := []string{routes[0].Host, routes[1].Host, routes[2].Host}
	want := []string{"a.localhost", "B.localhost", "c.localhost"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("routes sorted as %#v, want %#v", got, want)
	}
}

func TestAdminRoutesReadWaitsForStoreWriteLock(t *testing.T) {
	srv := NewServer(Config{Token: "secret", Store: NewMemoryStore()})
	srv.storeMu.Lock()

	done := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/routes", nil)
		req.Header.Set("X-Gohere-Token", "secret")
		rec := httptest.NewRecorder()
		srv.AdminHandler().ServeHTTP(rec, req)
		done <- rec.Code
	}()

	select {
	case code := <-done:
		t.Fatalf("GET /routes completed while store write lock was held with status %d", code)
	case <-time.After(50 * time.Millisecond):
	}
	srv.storeMu.Unlock()

	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("GET /routes = %d", code)
		}
	case <-time.After(time.Second):
		t.Fatal("GET /routes did not finish after store lock was released")
	}
}

func TestHTTPRouteLookupWaitsForStoreWriteLock(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	defer backend.Close()
	store := NewMemoryStore()
	if err := store.Save([]Route{{Host: "eventca.localhost", Target: backend.URL}}); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(Config{Token: "secret", Store: store})
	srv.storeMu.Lock()

	done := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodGet, "http://eventca.localhost/", nil)
		req.Host = "eventca.localhost"
		rec := httptest.NewRecorder()
		srv.HTTPHandler().ServeHTTP(rec, req)
		done <- rec.Code
	}()

	select {
	case code := <-done:
		t.Fatalf("HTTP route lookup completed while store write lock was held with status %d", code)
	case <-time.After(50 * time.Millisecond):
	}
	srv.storeMu.Unlock()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("HTTP route lookup did not finish after store lock was released")
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

func TestAdminAPIShutdownRequiresToken(t *testing.T) {
	called := false
	srv := NewServer(Config{Token: "secret", Store: NewMemoryStore(), Shutdown: func() { called = true }})

	req := httptest.NewRequest(http.MethodPost, "/shutdown", nil)
	rec := httptest.NewRecorder()
	srv.AdminHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /shutdown without token = %d", rec.Code)
	}
	if called {
		t.Fatal("shutdown should not be called without token")
	}
}

func TestAdminAPIShutdownCallsHandler(t *testing.T) {
	called := make(chan struct{}, 1)
	srv := NewServer(Config{Token: "secret", Store: NewMemoryStore(), Shutdown: func() { called <- struct{}{} }})

	req := httptest.NewRequest(http.MethodPost, "/shutdown", nil)
	req.Header.Set("X-Gohere-Token", "secret")
	rec := httptest.NewRecorder()
	srv.AdminHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST /shutdown = %d", rec.Code)
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("shutdown handler was not called")
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

func TestAdminAPIRouteStatusesComputedByRouter(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			t.Fatal("route status probe should not use GET")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	store := NewMemoryStore()
	if err := store.Save([]Route{{
		Host:   "squawk.localhost",
		Target: target.URL,
		CWD:    `D:\roie\dev\web\squawk`,
	}}); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(Config{Token: "secret", Store: store})

	req := httptest.NewRequest(http.MethodGet, "/route-statuses", nil)
	req.Header.Set("X-Gohere-Token", "secret")
	rec := httptest.NewRecorder()
	srv.AdminHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /route-statuses = %d body %q", rec.Code, rec.Body.String())
	}
	var statuses []RouteStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &statuses); err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Route.Host != "squawk.localhost" || statuses[0].Status != "ready" {
		t.Fatalf("statuses = %#v", statuses)
	}
}

func TestAdminAPIRouteStatusesRequiresToken(t *testing.T) {
	srv := NewServer(Config{Token: "secret", Store: NewMemoryStore()})

	req := httptest.NewRequest(http.MethodGet, "/route-statuses", nil)
	rec := httptest.NewRecorder()
	srv.AdminHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /route-statuses without token = %d", rec.Code)
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

func TestAdminAPIRoutesRejectsHugeBody(t *testing.T) {
	srv := NewServer(Config{Token: "secret", Store: NewMemoryStore()})

	body := &countingReader{Reader: strings.NewReader(strings.Repeat("x", maxAdminBodyBytes+1024))}
	req := httptest.NewRequest(http.MethodPost, "/routes", body)
	req.Header.Set("X-Gohere-Token", "secret")
	rec := httptest.NewRecorder()
	srv.AdminHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /routes = %d body %q", rec.Code, rec.Body.String())
	}
	if body.read > maxAdminBodyBytes {
		t.Fatalf("read %d bytes, want at most %d", body.read, maxAdminBodyBytes)
	}
}

func TestProxyWebSocketUpstreamErrorDoesNotReturnHTMLPage(t *testing.T) {
	store := NewMemoryStore()
	if err := store.Save([]Route{{
		Host:   "web.localhost",
		Target: "http://127.0.0.1:1",
	}}); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(Config{Token: "secret", Store: store})

	req := httptest.NewRequest(http.MethodGet, "http://web.localhost/socket", nil)
	req.Host = "web.localhost"
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	rec := httptest.NewRecorder()
	srv.HTTPHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("websocket error status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "<!doctype html>") {
		t.Fatalf("websocket error returned HTML page: %q", rec.Body.String())
	}
}

func TestProxyHTTPUpstreamErrorDoesNotReturnMissingRoutePage(t *testing.T) {
	store := NewMemoryStore()
	if err := store.Save([]Route{{
		Host:   "web.localhost",
		Target: "http://127.0.0.1:1",
	}}); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(Config{Token: "secret", Store: store})

	req := httptest.NewRequest(http.MethodGet, "http://web.localhost/", nil)
	req.Host = "web.localhost"
	rec := httptest.NewRecorder()
	srv.HTTPHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("upstream error status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "No gohere route is running") {
		t.Fatalf("upstream error returned missing route page: %q", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "gohere upstream unavailable") {
		t.Fatalf("upstream error body = %q", rec.Body.String())
	}
}

func TestProxyHTTPUpstreamErrorReturnsJSONForAPIClient(t *testing.T) {
	store := NewMemoryStore()
	if err := store.Save([]Route{{
		Host:   "web.localhost",
		Target: "http://127.0.0.1:1",
	}}); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(Config{Token: "secret", Store: store})

	req := httptest.NewRequest(http.MethodGet, "http://web.localhost/", nil)
	req.Host = "web.localhost"
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	srv.HTTPHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("upstream error status = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q, want application/json", got)
	}
	var payload proxyErrorPayload
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error != "upstream_unavailable" || payload.Host != "web.localhost" || payload.Target != "http://127.0.0.1:1" {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestMissingRoutePageStripsIPv6Port(t *testing.T) {
	rec := httptest.NewRecorder()

	missingRoutePage(rec, "[::1]:8080")

	if !strings.Contains(rec.Body.String(), "No gohere route is running for ::1") {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "for [") {
		t.Fatalf("body kept bracket fragment: %q", rec.Body.String())
	}
}

func TestAdminAPIProbeTargetRejectsPublicHosts(t *testing.T) {
	srv := NewServer(Config{Token: "secret", Store: NewMemoryStore()})

	req := httptest.NewRequest(http.MethodPost, "/probe-target", strings.NewReader(`{"target":"https://example.com"}`))
	req.Header.Set("X-Gohere-Token", "secret")
	rec := httptest.NewRecorder()
	srv.AdminHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /probe-target = %d body %q", rec.Code, rec.Body.String())
	}
}

func TestAdminAPIRejectsRouteWithPublicTarget(t *testing.T) {
	srv := NewServer(Config{Token: "secret", Store: NewMemoryStore()})

	body := strings.NewReader(`{"host":"app.localhost","target":"https://example.com"}`)
	req := httptest.NewRequest(http.MethodPost, "/routes", body)
	req.Header.Set("X-Gohere-Token", "secret")
	rec := httptest.NewRecorder()
	srv.AdminHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /routes = %d body %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "target must be local") {
		t.Fatalf("POST /routes body = %q, want local target error", rec.Body.String())
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

func TestAdminAPIConcurrentUpsertsDoNotLoseRoutes(t *testing.T) {
	store := NewMemoryStore()
	srv := NewServer(Config{Token: "secret", Store: store})
	handler := srv.AdminHandler()
	const count = 20
	done := make(chan error, count)

	for i := 0; i < count; i++ {
		go func(i int) {
			body := strings.NewReader(fmt.Sprintf(`{"host":"app-%d.localhost","target":"http://127.0.0.1:%d"}`, i, 40000+i))
			req := httptest.NewRequest(http.MethodPost, "/routes", body)
			req.Header.Set("X-Gohere-Token", "secret")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusNoContent {
				done <- fmt.Errorf("POST route %d = %d body %q", i, rec.Code, rec.Body.String())
				return
			}
			done <- nil
		}(i)
	}

	for i := 0; i < count; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	routes, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != count {
		t.Fatalf("routes = %d, want %d: %#v", len(routes), count, routes)
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

func TestAdminAPIDeleteUnescapesHostPathSegment(t *testing.T) {
	store := NewMemoryStore()
	store.Save([]Route{{Host: "weird/host?x#y", Target: "http://127.0.0.1:49231"}})
	srv := NewServer(Config{Token: "secret", Store: store})

	req := httptest.NewRequest(http.MethodDelete, "/routes/weird%2Fhost%3Fx%23y", nil)
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

func TestProxyAddsRouteHopHeader(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend-Route-Hop", r.Header.Get(gohereRouteHeader))
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
	if rec.Header().Get("X-Backend-Route-Hop") != "eventca.localhost" {
		t.Fatalf("route hop header = %q, want eventca.localhost", rec.Header().Get("X-Backend-Route-Hop"))
	}
}

func TestProxyAllowsDifferentRouteHopHeader(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend-Route-Hop", strings.Join(r.Header.Values(gohereRouteHeader), ","))
		io.WriteString(w, "proxied response")
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Save([]Route{{Host: "worker.localhost", Target: backend.URL}})
	srv := NewServer(Config{Token: "secret", Store: store})

	req := httptest.NewRequest(http.MethodGet, "http://worker.localhost/", nil)
	req.Host = "worker.localhost"
	req.Header.Set(gohereRouteHeader, "web.localhost")
	rec := httptest.NewRecorder()
	srv.HTTPHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("proxy status = %d body %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Backend-Route-Hop") != "web.localhost,worker.localhost" {
		t.Fatalf("route hop header = %q, want web.localhost,worker.localhost", rec.Header().Get("X-Backend-Route-Hop"))
	}
}

func TestProxyDetectsRouteLoop(t *testing.T) {
	upstreamHit := false
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit = true
		io.WriteString(w, "unexpected upstream")
	}))
	defer backend.Close()

	store := NewMemoryStore()
	store.Save([]Route{{Host: "web.localhost", Target: backend.URL}})
	srv := NewServer(Config{Token: "secret", Store: store})

	req := httptest.NewRequest(http.MethodGet, "http://web.localhost/api", nil)
	req.Host = "web.localhost"
	req.Header.Set(gohereRouteHeader, "web.localhost")
	rec := httptest.NewRecorder()
	srv.HTTPHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusLoopDetected {
		t.Fatalf("proxy status = %d body %q", rec.Code, rec.Body.String())
	}
	if upstreamHit {
		t.Fatal("looping request should not hit upstream")
	}
	if !strings.Contains(rec.Body.String(), "gohere proxy loop detected") ||
		!strings.Contains(rec.Body.String(), "changeOrigin: true") {
		t.Fatalf("loop response body = %q", rec.Body.String())
	}
}

func TestProxyLoopReturnsJSONForAPIClient(t *testing.T) {
	store := NewMemoryStore()
	store.Save([]Route{{Host: "web.localhost", Target: "http://127.0.0.1:1"}})
	srv := NewServer(Config{Token: "secret", Store: store})

	req := httptest.NewRequest(http.MethodGet, "http://web.localhost/api", nil)
	req.Host = "web.localhost"
	req.Header.Set(gohereRouteHeader, "web.localhost")
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	srv.HTTPHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusLoopDetected {
		t.Fatalf("proxy status = %d body %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q, want application/json", got)
	}
	var payload proxyErrorPayload
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error != "proxy_loop_detected" || payload.Host != "web.localhost" ||
		!strings.Contains(payload.Message, "gohere proxy loop detected") {
		t.Fatalf("payload = %#v", payload)
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
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()

	srv.HTTPHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("missing route status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "No gohere route is running for missing.localhost") {
		t.Fatalf("missing route body = %q", rec.Body.String())
	}
}

func TestMissingRouteReturnsPlainTextForNonBrowserClient(t *testing.T) {
	srv := NewServer(Config{Token: "secret", Store: NewMemoryStore()})
	req := httptest.NewRequest(http.MethodGet, "http://missing.localhost/", nil)
	req.Host = "missing.localhost"
	req.Header.Set("Accept", "*/*")
	rec := httptest.NewRecorder()

	srv.HTTPHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("missing route status = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("content type = %q, want text/plain", got)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<!doctype html>") || !strings.Contains(body, "No gohere route is running for missing.localhost") {
		t.Fatalf("missing route body = %q", body)
	}
}

func TestMissingRouteReturnsJSONForAPIClient(t *testing.T) {
	srv := NewServer(Config{Token: "secret", Store: NewMemoryStore()})
	req := httptest.NewRequest(http.MethodGet, "http://missing.localhost/", nil)
	req.Host = "missing.localhost"
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()

	srv.HTTPHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("missing route status = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q, want application/json", got)
	}
	var payload proxyErrorPayload
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error != "route_missing" || payload.Host != "missing.localhost" ||
		payload.Message != "No gohere route is running for missing.localhost" {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestMissingRouteReturnsJSONForStructuredJSONClient(t *testing.T) {
	srv := NewServer(Config{Token: "secret", Store: NewMemoryStore()})
	req := httptest.NewRequest(http.MethodGet, "http://missing.localhost/", nil)
	req.Host = "missing.localhost"
	req.Header.Set("Accept", "application/problem+json")
	rec := httptest.NewRecorder()

	srv.HTTPHandler().ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q, want application/json", got)
	}
	var payload proxyErrorPayload
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error != "route_missing" {
		t.Fatalf("payload = %#v", payload)
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

func TestRotateLogRestoresCurrentLogWhenReplacementCreateFails(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "router.log")
	original := bytes.Repeat([]byte("x"), maxLogSize+1)
	if err := os.WriteFile(logPath, original, 0644); err != nil {
		t.Fatal(err)
	}

	oldOpenFile := rotateOpenFile
	defer func() {
		rotateOpenFile = oldOpenFile
	}()
	rotateOpenFile = func(name string, flag int, perm os.FileMode) (*os.File, error) {
		if name == logPath {
			return nil, errors.New("create failed")
		}
		return oldOpenFile(name, flag, perm)
	}

	err := RotateLog(logPath)
	if err == nil || !strings.Contains(err.Error(), "create failed") {
		t.Fatalf("RotateLog error = %v", err)
	}
	current, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(current, original) {
		t.Fatalf("current log was not restored")
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

func TestStartConfiguresHTTPServerTimeouts(t *testing.T) {
	running, err := Start(t.Context(), StartConfig{
		HTTPAddr:  "127.0.0.1:0",
		AdminAddr: "127.0.0.1:0",
		StateDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer running.Close()

	if running.httpServer.ReadHeaderTimeout == 0 {
		t.Fatalf("http server timeouts = readHeader:%s read:%s write:%s idle:%s",
			running.httpServer.ReadHeaderTimeout,
			running.httpServer.ReadTimeout,
			running.httpServer.WriteTimeout,
			running.httpServer.IdleTimeout)
	}
	if running.adminServer.ReadHeaderTimeout == 0 ||
		running.adminServer.ReadTimeout == 0 ||
		running.adminServer.WriteTimeout == 0 ||
		running.adminServer.IdleTimeout == 0 {
		t.Fatalf("admin server timeouts = readHeader:%s read:%s write:%s idle:%s",
			running.adminServer.ReadHeaderTimeout,
			running.adminServer.ReadTimeout,
			running.adminServer.WriteTimeout,
			running.adminServer.IdleTimeout)
	}
}

func TestDefaultHTTPAddrUsesDarwinWildcard(t *testing.T) {
	if got := defaultHTTPAddrForGOOS("darwin"); got != "[::]:80" {
		t.Fatalf("darwin default HTTP addr = %q, want [::]:80", got)
	}
	if got := defaultHTTPAddrForGOOS("linux"); got != "127.0.0.1:80" {
		t.Fatalf("linux default HTTP addr = %q, want 127.0.0.1:80", got)
	}
	if got := defaultHTTPAddrForGOOS("windows"); got != "127.0.0.1:80" {
		t.Fatalf("windows default HTTP addr = %q, want 127.0.0.1:80", got)
	}
}

func TestLoopbackOnlyListenerRejectsNonLoopbackBeforeHTTP(t *testing.T) {
	rejected := &fakeConn{remote: tcpAddr("192.168.1.187")}
	accepted := &fakeConn{remote: tcpAddr("127.0.0.1")}
	listener := &fakeListener{conns: []net.Conn{rejected, accepted}}

	conn, err := (loopbackOnlyListener{Listener: listener}).Accept()
	if err != nil {
		t.Fatal(err)
	}
	if conn != accepted {
		t.Fatalf("accepted conn = %#v, want loopback conn", conn)
	}
	if !rejected.closed {
		t.Fatal("non-loopback connection was not closed")
	}
	if accepted.closed {
		t.Fatal("loopback connection should not be closed")
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

func TestShutdownClosesRunningDone(t *testing.T) {
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

	token, err := ReadToken(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+running.AdminAddr+"/shutdown", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Gohere-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /shutdown = %d", resp.StatusCode)
	}

	select {
	case <-running.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("running Done was not closed after shutdown")
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

type countingReader struct {
	*strings.Reader
	read int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.read += int64(n)
	return n, err
}

type fakeListener struct {
	conns []net.Conn
}

func (l *fakeListener) Accept() (net.Conn, error) {
	if len(l.conns) == 0 {
		return nil, io.EOF
	}
	conn := l.conns[0]
	l.conns = l.conns[1:]
	return conn, nil
}

func (l *fakeListener) Close() error {
	return nil
}

func (l *fakeListener) Addr() net.Addr {
	return tcpAddr("127.0.0.1")
}

type fakeConn struct {
	remote net.Addr
	closed bool
}

func (c *fakeConn) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (c *fakeConn) Write(p []byte) (int, error) {
	return len(p), nil
}

func (c *fakeConn) Close() error {
	c.closed = true
	return nil
}

func (c *fakeConn) LocalAddr() net.Addr {
	return tcpAddr("127.0.0.1")
}

func (c *fakeConn) RemoteAddr() net.Addr {
	return c.remote
}

func (c *fakeConn) SetDeadline(time.Time) error {
	return nil
}

func (c *fakeConn) SetReadDeadline(time.Time) error {
	return nil
}

func (c *fakeConn) SetWriteDeadline(time.Time) error {
	return nil
}

func tcpAddr(ip string) *net.TCPAddr {
	return &net.TCPAddr{IP: net.ParseIP(ip), Port: 12345}
}
