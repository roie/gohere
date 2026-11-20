package router

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestStateFileWritesReplaceExistingFiles(t *testing.T) {
	for _, test := range []struct {
		name  string
		write func(string) (string, error)
	}{
		{"token", func(path string) (string, error) {
			token, err := writeToken(path)
			return token + "\n", err
		}},
		{"pid", func(path string) (string, error) {
			return "12345\n", writePIDFile(path, 12345)
		}},
		{"instance", func(path string) (string, error) {
			return "new-instance\n", writeRouterInstance(path, "new-instance")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, test.name)
			if err := os.WriteFile(path, []byte("old\n"), 0600); err != nil {
				t.Fatal(err)
			}
			want, err := test.write(path)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != want {
				t.Fatalf("file = %q, err = %v, want %q", data, err, want)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
				t.Fatalf("permissions = %v, want 0600", info.Mode().Perm())
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 {
				t.Fatalf("directory entries = %v, err = %v, want only destination", entries, err)
			}
		})
	}
}

func TestStateFileReplacementFailuresCleanUpTemporaryFiles(t *testing.T) {
	for _, test := range []struct {
		name  string
		write func(string) error
	}{
		{"token", func(path string) error { _, err := writeToken(path); return err }},
		{"routes", func(path string) error { return NewRouteStore(path).save(nil) }},
		{"pid", func(path string) error { return writePIDFile(path, 12345) }},
		{"instance", func(path string) error { return writeRouterInstance(path, "new-instance") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, test.name)
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			original := filepath.Join(path, "original")
			if err := os.WriteFile(original, []byte("old\n"), 0600); err != nil {
				t.Fatal(err)
			}
			err := test.write(path)
			if _, ok := errors.AsType[*os.LinkError](err); !ok {
				t.Fatalf("err = %v, want rename error replacing directory", err)
			}
			data, err := os.ReadFile(original)
			if err != nil || string(data) != "old\n" {
				t.Fatalf("original = %q, err = %v", data, err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 || entries[0].Name() != test.name {
				t.Fatalf("directory entries = %v, err = %v, want only original directory", entries, err)
			}
		})
	}
}

func TestRouteStoreSaveReplacesExistingRoutes(t *testing.T) {
	dir := t.TempDir()
	store := NewRouteStore(filepath.Join(dir, RoutesFilename))
	for _, host := range []string{"old.localhost", "new.localhost"} {
		if err := store.Save([]Route{{Host: host, Target: "http://127.0.0.1:5173"}}); err != nil {
			t.Fatal(err)
		}
	}
	routes, err := store.Load()
	if err != nil || len(routes) != 1 || routes[0].Host != "new.localhost" {
		t.Fatalf("routes = %#v, err = %v", routes, err)
	}
	info, err := os.Stat(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("permissions = %v, want 0600", info.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("directory entries = %v, err = %v, want routes and lock", entries, err)
	}
}

func TestRouteStoreFailedReplacementPreservesRoutes(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows rejects replacing read-only or open destinations")
	}
	for _, mode := range []string{"read-only", "open"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			store := NewRouteStore(filepath.Join(dir, RoutesFilename))
			if err := store.Save([]Route{{Host: "old.localhost", Target: "http://127.0.0.1:5173"}}); err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(store.path)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "read-only" {
				if err := os.Chmod(store.path, 0400); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Chmod(store.path, 0600); err != nil {
						t.Error(err)
					}
				})
			} else {
				file, err := os.Open(store.path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := file.Close(); err != nil {
						t.Error(err)
					}
				})
			}
			err = store.Save([]Route{{Host: "new.localhost"}})
			if _, ok := errors.AsType[*os.LinkError](err); !ok {
				t.Fatalf("err = %v, want rename error replacing %s routes", err, mode)
			}
			data, err := os.ReadFile(store.path)
			if err != nil || !bytes.Equal(data, original) {
				t.Fatalf("routes file changed after failed replacement: %q, err = %v", data, err)
			}
			routes, err := store.Load()
			if err != nil || len(routes) != 1 || routes[0].Host != "old.localhost" {
				t.Fatalf("routes = %#v, err = %v", routes, err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 2 {
				t.Fatalf("directory entries = %v, err = %v, want original routes and lock", entries, err)
			}
		})
	}
}
