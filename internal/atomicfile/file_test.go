package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteCreatesAndReplacesFile(t *testing.T) {
	for _, test := range []struct {
		name string
		mode os.FileMode
	}{
		{"private", 0600},
		{"public", 0644},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "state")
			for _, want := range []string{"first\n", "replacement\n"} {
				if err := Write(path, []byte(want), test.mode); err != nil {
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
				if runtime.GOOS != "windows" && info.Mode().Perm() != test.mode {
					t.Fatalf("permissions = %v, want %v", info.Mode().Perm(), test.mode)
				}
				entries, err := os.ReadDir(dir)
				if err != nil || len(entries) != 1 {
					t.Fatalf("directory entries = %v, err = %v, want only destination", entries, err)
				}
			}
		})
	}
}

func TestWriteRequiresExistingParent(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "missing")
	if err := Write(filepath.Join(parent, "state"), []byte("data"), 0600); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want missing-parent error", err)
	}
	if _, err := os.Stat(parent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("parent stat err = %v, want not exist", err)
	}
}

func TestWriteRenameFailureCleansUpTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(path, "original")
	if err := os.WriteFile(original, []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	err := Write(path, []byte("new\n"), 0600)
	renameErr, ok := errors.AsType[*os.LinkError](err)
	if !ok || renameErr.Op != "rename" || renameErr.New != path {
		t.Fatalf("err = %v, want rename error for %s", err, path)
	}
	data, err := os.ReadFile(original)
	if err != nil || string(data) != "old\n" {
		t.Fatalf("original = %q, err = %v", data, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "state" {
		t.Fatalf("directory entries = %v, err = %v, want only original directory", entries, err)
	}
}
