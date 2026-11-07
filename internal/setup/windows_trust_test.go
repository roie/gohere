package setup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type windowsTrustRunner struct {
	detachingRunner
	trusted bool
	imports int
	checks  int
}

func (r *windowsTrustRunner) Run(ctx context.Context, command string, args ...string) error {
	if command == "certutil" && len(args) > 1 {
		if args[1] == "-store" {
			r.checks++
			if !r.trusted {
				return errors.New("certificate not found")
			}
			return nil
		}
		if args[1] == "-user" {
			r.imports++
			r.trusted = true
			return nil
		}
	}
	return r.detachingRunner.Run(ctx, command, args...)
}

func TestWindowsHTTPSUpgradePreservesCAAndSkipsExistingTrust(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.exe")
	if err := os.WriteFile(source, []byte("old binary"), 0755); err != nil {
		t.Fatal(err)
	}
	runner := &windowsTrustRunner{detachingRunner: detachingRunner{pid: 4242}}
	cfg := Config{
		StateDir: filepath.Join(dir, "state"), CurrentBinary: source, CommandRunner: runner, HTTPS: true,
		EnsureUserPath: func(context.Context, string) error { return nil },
	}
	if err := Windows(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(cfg.StateDir, "ca", "ca.pem")
	keyPath := filepath.Join(cfg.StateDir, "ca", "ca.key")
	certificate, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatal(err)
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("new binary"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := Windows(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if runner.imports != 1 || runner.checks != 2 {
		t.Fatalf("trust imports/checks = %d/%d, want 1/2", runner.imports, runner.checks)
	}
	for path, want := range map[string][]byte{caPath: certificate, keyPath: key} {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("upgrade changed %s: %v", path, err)
		}
	}
	installed, err := os.ReadFile(filepath.Join(cfg.StateDir, "bin", "gohere.exe"))
	if err != nil || string(installed) != "new binary" {
		t.Fatalf("upgraded binary = %q, %v", installed, err)
	}
}
