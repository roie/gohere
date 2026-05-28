package staticserver

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDetectStaticProjectRequiresIndexHTML(t *testing.T) {
	dir := t.TempDir()
	if IsStaticProject(dir) {
		t.Fatal("empty dir should not be static project")
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>Hello</h1>"), 0644); err != nil {
		t.Fatal(err)
	}
	if !IsStaticProject(dir) {
		t.Fatal("index.html should be static project")
	}
}

func TestHandlerServesIndexAndFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>Hello</h1>"), 0644)
	os.WriteFile(filepath.Join(dir, "app.js"), []byte("console.log('hi')"), 0644)
	handler := Handler(dir)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "<h1>Hello</h1>" {
		t.Fatalf("index response = %d %q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/app.js", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "console.log('hi')" {
		t.Fatalf("file response = %d %q", rec.Code, rec.Body.String())
	}
}

func TestHandlerDoesNotExposeDirectoryListings(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>Hello</h1>"), 0644)
	os.Mkdir(filepath.Join(dir, "assets"), 0755)
	handler := Handler(dir)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("directory response = %d %q", rec.Code, rec.Body.String())
	}
}

func TestHandlerServesDirectoryIndex(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "docs"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "index.html"), []byte("<h1>Docs</h1>"), 0644); err != nil {
		t.Fatal(err)
	}
	handler := Handler(dir)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/docs/", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "<h1>Docs</h1>" {
		t.Fatalf("directory index response = %d %q", rec.Code, rec.Body.String())
	}
}

func TestLiveHandlerInjectsReloadClientIntoHTML(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html><body><h1>Hello</h1></body></html>"), 0644); err != nil {
		t.Fatal(err)
	}
	handler := HandlerWithConfig(Config{Dir: dir, Live: true})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "<h1>Hello</h1>") {
		t.Fatalf("html response = %d %q", rec.Code, body)
	}
	if !strings.Contains(body, `EventSource("/__gohere/live")`) {
		t.Fatalf("live client was not injected into html: %q", body)
	}
	if !strings.Contains(body, "</script></body>") {
		t.Fatalf("live client should be injected before body close: %q", body)
	}
}

func TestLiveHandlerDoesNotInjectIntoNonHTML(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("console.log('hi')"), 0644); err != nil {
		t.Fatal(err)
	}
	handler := HandlerWithConfig(Config{Dir: dir, Live: true})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/app.js", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "console.log('hi')" {
		t.Fatalf("js response = %d %q", rec.Code, rec.Body.String())
	}
}

func TestLiveServerEmitsReloadEventOnFileChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.html")
	if err := os.WriteFile(path, []byte("<h1>Hello</h1>"), 0644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	server, err := StartWithConfig(ctx, Config{Dir: dir, Live: true, PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+server.PortString()+"/__gohere/live", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("content type = %q, want text/event-stream", got)
	}

	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, ": connected") {
		t.Fatalf("first SSE line = %q, want connected comment", line)
	}

	if err := os.WriteFile(path, []byte("<h1>Changed</h1>"), 0644); err != nil {
		t.Fatal(err)
	}

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading SSE event: %v", err)
		}
		if strings.HasPrefix(line, "data: reload") {
			return
		}
	}
}

func TestHandlerDoesNotServePathTraversalOutsideRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "site")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("<h1>Hello</h1>"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "secret.txt"), []byte("secret"), 0644); err != nil {
		t.Fatal(err)
	}
	handler := Handler(root)

	for _, path := range []string{"/../secret.txt", "/%2e%2e/secret.txt", "/%5c..%5csecret.txt"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("path %q response = %d %q", path, rec.Code, rec.Body.String())
		}
	}
}

func TestStartServesOnHiddenPort(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>Hello</h1>"), 0644)

	server, err := Start(t.Context(), dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	resp, err := http.Get("http://127.0.0.1:" + server.PortString() + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if string(data) != "<h1>Hello</h1>" {
		t.Fatalf("body = %q", string(data))
	}
}
