package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	localcert "github.com/roie/gohere/internal/cert"
	"github.com/roie/gohere/internal/companion"
	appconfig "github.com/roie/gohere/internal/config"
	"github.com/roie/gohere/internal/router"
)

func TestServeCompanionRejectsNonWindowsAuthority(t *testing.T) {
	input := strings.NewReader(`{"magic":"gohere-companion","protocolVersion":1,"operation":"info"}`)
	var output bytes.Buffer
	if err := ServeCompanion(t.Context(), input, &output, CompanionConfig{GOOS: "linux"}); err != nil {
		t.Fatal(err)
	}
	var response companion.Response
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.OK || response.Error == nil || !strings.Contains(response.Error.Message, "requires Windows") {
		t.Fatalf("response = %#v", response)
	}
}

func TestCompanionInfoReportsWindowsAuthorityWithoutSecrets(t *testing.T) {
	stateDir := t.TempDir()
	writeCompanionTestFile(t, filepath.Join(stateDir, "bin", "gohere.exe"), "binary")
	writeCompanionTestFile(t, filepath.Join(stateDir, "token"), "never-cross-this-boundary")
	writeCompanionTestFile(t, filepath.Join(stateDir, "router.pid"), "4321\n")
	ca, err := (localcert.Store{StateDir: stateDir}).EnsureCA()
	if err != nil {
		t.Fatal(err)
	}
	authority := newCompanionAuthority(CompanionConfig{
		Version:     "1.2.3",
		GOOS:        "windows",
		GOARCH:      "amd64",
		StateDir:    stateDir,
		User:        `DESKTOP\Alice`,
		UserProfile: `C:\Users\Alice`,
		AdminClient: func() (adminClient, error) { return fakeAdminClient{}, nil },
	})

	info, err := authority.Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !info.RouterInstalled || !info.RouterReady || info.RouterInstanceID != "legacy-pid:4321" {
		t.Fatalf("info = %#v", info)
	}
	if info.CAFingerprint != ca.Fingerprint || !hasCompanionCapability(info, "control.bootstrap") ||
		!hasCompanionCapability(info, "control.ca-certificate") || !hasCompanionCapability(info, "control.health") {
		t.Fatalf("info = %#v", info)
	}
	encoded, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"never-cross-this-boundary", "ca.key", "token"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("info leaked %q: %s", secret, encoded)
		}
	}
}

func TestCompanionBootstrapCapturesProgress(t *testing.T) {
	called := false
	authority := newCompanionAuthority(CompanionConfig{
		GOOS: "windows",
		Bootstrap: func(_ context.Context, enableHTTPS bool, output io.Writer) error {
			called = enableHTTPS
			fmt.Fprintln(output, "installed Windows router")
			return nil
		},
	})

	output, err := authority.Bootstrap(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !called || output != "installed Windows router\n" {
		t.Fatalf("called = %v, output = %q", called, output)
	}
}

func TestCompanionReadyInfoStartsInstalledRouterInsideOneOperation(t *testing.T) {
	stateDir := t.TempDir()
	writeCompanionTestFile(t, filepath.Join(stateDir, "bin", "gohere.exe"), "binary")
	writeCompanionTestFile(t, filepath.Join(stateDir, "token"), "token")
	writeCompanionTestFile(t, filepath.Join(stateDir, router.RouterInstanceFilename), "router-1\n")
	ready := false
	starts := 0
	authority := newCompanionAuthority(CompanionConfig{
		GOOS:     "windows",
		StateDir: stateDir,
		AdminClient: func() (adminClient, error) {
			return readinessAdminClient{ready: &ready}, nil
		},
		StartRouter: func(context.Context) error {
			starts++
			ready = true
			return nil
		},
	})

	info, err := authority.ReadyInfo(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if starts != 1 || !info.RouterReady || info.RouterInstanceID != "router-1" {
		t.Fatalf("starts = %d, info = %#v", starts, info)
	}
}

func TestReleaseVersionComparisonPreventsWindowsDowngrade(t *testing.T) {
	newer, ok := parseReleaseVersion("gohere 1.4.0\n")
	if !ok {
		t.Fatal("newer version did not parse")
	}
	older, ok := parseReleaseVersion("v1.3.9")
	if !ok {
		t.Fatal("older version did not parse")
	}
	if compareReleaseVersions(newer, older) <= 0 || compareReleaseVersions(older, newer) >= 0 {
		t.Fatalf("comparison = newer/older %d, older/newer %d", compareReleaseVersions(newer, older), compareReleaseVersions(older, newer))
	}
	if _, ok := parseReleaseVersion("dev"); ok {
		t.Fatal("development version must not participate in automatic replacement")
	}
	if !shouldUpgradeWindowsBinary("1.4.0", "gohere 1.3.9\n") {
		t.Fatal("newer companion should upgrade the installed Windows binary")
	}
	if !shouldUpgradeWindowsBinary("1.4.0", "unknown") {
		t.Fatal("release companion should replace an unversioned development binary")
	}
	for _, test := range []struct {
		current   string
		installed string
	}{
		{current: "1.3.9", installed: "gohere 1.4.0"},
		{current: "1.4.0", installed: "gohere 1.4.0"},
		{current: "dev", installed: "gohere 1.3.9"},
	} {
		if shouldUpgradeWindowsBinary(test.current, test.installed) {
			t.Fatalf("unexpected upgrade from installed %q using %q", test.installed, test.current)
		}
	}
}

func TestCompanionReadyInfoUpgradesOlderWindowsBinaryWithoutChangingHTTPSPolicy(t *testing.T) {
	stateDir := t.TempDir()
	stableBinary := filepath.Join(stateDir, "bin", "gohere.exe")
	writeCompanionTestFile(t, stableBinary, "#!/bin/sh\nprintf 'gohere 1.2.3\\n'\n")
	if err := os.Chmod(stableBinary, 0755); err != nil {
		t.Fatal(err)
	}
	writeCompanionTestFile(t, filepath.Join(stateDir, "token"), "token")
	writeCompanionTestFile(t, filepath.Join(stateDir, router.RouterInstanceFilename), "router-1\n")
	if err := appconfig.Save(stateDir, appconfig.Config{HTTPS: false}); err != nil {
		t.Fatal(err)
	}
	bootstrapCalls := 0
	requestedHTTPS := true
	authority := newCompanionAuthority(CompanionConfig{
		Version:  "1.3.0",
		GOOS:     "windows",
		StateDir: stateDir,
		AdminClient: func() (adminClient, error) {
			return fakeAdminClient{}, nil
		},
		Bootstrap: func(_ context.Context, enableHTTPS bool, _ io.Writer) error {
			bootstrapCalls++
			requestedHTTPS = enableHTTPS
			return nil
		},
	})

	if _, err := authority.ReadyInfo(t.Context()); err != nil {
		t.Fatal(err)
	}
	if bootstrapCalls != 1 || requestedHTTPS {
		t.Fatalf("bootstrap calls = %d, requested HTTPS = %v", bootstrapCalls, requestedHTTPS)
	}
}

func TestCompanionReturnsOnlyPublicCACertificate(t *testing.T) {
	stateDir := t.TempDir()
	store := localcert.Store{StateDir: stateDir}
	if _, err := store.EnsureCA(); err != nil {
		t.Fatal(err)
	}
	authority := newCompanionAuthority(CompanionConfig{GOOS: "windows", StateDir: stateDir})

	certificate, err := authority.CACertificate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(certificate, "BEGIN CERTIFICATE") || strings.Contains(certificate, "PRIVATE KEY") {
		t.Fatalf("certificate = %q", certificate)
	}
}

func writeCompanionTestFile(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}

type readinessAdminClient struct {
	ready *bool
}

func (c readinessAdminClient) Health(context.Context) error {
	if c.ready != nil && *c.ready {
		return nil
	}
	return errors.New("router stopped")
}

func (readinessAdminClient) Routes(context.Context) ([]router.Route, error) {
	return nil, nil
}

func (readinessAdminClient) UpsertRoute(context.Context, router.Route) error {
	return nil
}

func (readinessAdminClient) DeleteRoute(context.Context, string) error {
	return nil
}
