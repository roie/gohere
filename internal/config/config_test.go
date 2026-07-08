package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMissingConfigDefaultsHTTPSDisabled(t *testing.T) {
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPS {
		t.Fatalf("HTTPS = true, want false for missing config")
	}
}

func TestSaveAndLoadHTTPSConfig(t *testing.T) {
	dir := t.TempDir()

	if err := Save(dir, Config{HTTPS: true}); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.HTTPS {
		t.Fatalf("HTTPS = false, want true")
	}

	info, err := os.Stat(filepath.Join(dir, Filename))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("config permissions = %v, want 0600", info.Mode().Perm())
	}
}

func TestLoadInvalidConfigReportsPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, Filename)
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected invalid config error")
	}
	if got := err.Error(); got == "" || !strings.Contains(got, path) {
		t.Fatalf("error = %q, want path %q", got, path)
	}
}
