package staticserver

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
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
