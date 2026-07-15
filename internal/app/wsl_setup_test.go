package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	localcert "github.com/roie/gohere/internal/cert"
	"github.com/roie/gohere/internal/companion"
	"github.com/roie/gohere/internal/router"
)

func TestInstallWSLIntegrationKeepsOnlyEdgeAndPublicAuthorityState(t *testing.T) {
	certificate, fingerprint := testCA(t)
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	executable := filepath.Join(root, "npm", "gohere")
	if err := os.MkdirAll(filepath.Dir(executable), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("linux-cli"), 0755); err != nil {
		t.Fatal(err)
	}
	runner := &wslSetupRecordingRunner{}
	info := companion.Info{
		CompanionVersion: "1.2.3",
		User:             `DESKTOP\Alice`,
		StateDir:         `C:\Users\Alice\.gohere`,
		RouterInstanceID: "router-1",
		CAFingerprint:    fingerprint,
	}

	err := installWSLIntegration(t.Context(), info, certificate, wslIntegrationSetupConfig{
		StateDir:          stateDir,
		CurrentExecutable: executable,
		Distro:            "Ubuntu",
		LinuxUser:         "alice",
		Runner:            runner,
		Input:             strings.NewReader(""),
	})
	if err != nil {
		t.Fatal(err)
	}

	integrationDir := filepath.Join(stateDir, wslIntegrationDirname)
	edgeSum := sha256.Sum256([]byte("linux-cli"))
	edgeHash := hex.EncodeToString(edgeSum[:])
	edgeBinary := filepath.Join(integrationDir, "bin", wslEdgeBinaryName+"-"+edgeHash)
	data, err := os.ReadFile(edgeBinary)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "linux-cli" {
		t.Fatalf("edge binary = %q", data)
	}
	caData, err := os.ReadFile(filepath.Join(integrationDir, "windows-ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if string(caData) != certificate {
		t.Fatal("stored public CA differs from Windows CA")
	}

	var metadata wslIntegrationMetadata
	metadataData, err := os.ReadFile(filepath.Join(integrationDir, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(metadataData, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.WindowsStateDir != info.StateDir || metadata.RouterInstanceID != info.RouterInstanceID ||
		metadata.Distro != "Ubuntu" || metadata.LinuxUser != "alice" || metadata.CAFingerprint != fingerprint ||
		metadata.CompanionVersion != info.CompanionVersion || metadata.EdgeBinary != edgeBinary ||
		metadata.EdgeSHA256 != edgeHash || len(metadata.OwnerInstance) != 32 {
		t.Fatalf("metadata = %#v", metadata)
	}

	wantCommands := [][]string{
		{"sudo", "setcap", "cap_net_bind_service=+ep", edgeBinary},
		{"sudo", "mkdir", "-p", "/usr/local/share/ca-certificates"},
		{"sudo", "cp", filepath.Join(integrationDir, "windows-ca.pem"), "/usr/local/share/ca-certificates/gohere-local-ca.crt"},
		{"sudo", "update-ca-certificates"},
	}
	if !reflect.DeepEqual(runner.commands, wantCommands) {
		t.Fatalf("commands = %#v, want %#v", runner.commands, wantCommands)
	}

	for _, forbidden := range []string{
		filepath.Join(stateDir, "token"),
		filepath.Join(stateDir, "routes.json"),
		filepath.Join(stateDir, "ca", "ca.key"),
		filepath.Join(stateDir, "bin", "gohere"),
	} {
		if _, err := os.Stat(forbidden); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("forbidden WSL authority file exists: %s", forbidden)
		}
	}
}

func TestInstallWSLIntegrationKeepsPreviousEdgeCandidate(t *testing.T) {
	certificate, fingerprint := testCA(t)
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	executable := filepath.Join(root, "gohere")
	if err := os.WriteFile(executable, []byte("first-edge"), 0755); err != nil {
		t.Fatal(err)
	}
	cfg := wslIntegrationSetupConfig{
		StateDir:          stateDir,
		CurrentExecutable: executable,
		Runner:            &wslSetupRecordingRunner{},
		Input:             strings.NewReader(""),
	}
	info := companion.Info{CompanionVersion: "1.2.3", CAFingerprint: fingerprint}
	if err := installWSLIntegration(t.Context(), info, certificate, cfg); err != nil {
		t.Fatal(err)
	}
	firstSum := sha256.Sum256([]byte("first-edge"))
	firstPath := filepath.Join(stateDir, wslIntegrationDirname, "bin", wslEdgeBinaryName+"-"+hex.EncodeToString(firstSum[:]))

	if err := os.WriteFile(executable, []byte("second-edge"), 0755); err != nil {
		t.Fatal(err)
	}
	cfg.Runner = &wslSetupRecordingRunner{}
	if err := installWSLIntegration(t.Context(), info, certificate, cfg); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(firstPath); err != nil || string(data) != "first-edge" {
		t.Fatalf("previous edge candidate = %q, err = %v", data, err)
	}
}

func TestInstallWSLIntegrationRepairsExecutePermissionOnReusedCandidate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not preserve Unix execute bits")
	}
	certificate, fingerprint := testCA(t)
	root := t.TempDir()
	executable := filepath.Join(root, "gohere")
	if err := os.WriteFile(executable, []byte("edge"), 0755); err != nil {
		t.Fatal(err)
	}
	cfg := wslIntegrationSetupConfig{
		StateDir:          filepath.Join(root, "state"),
		CurrentExecutable: executable,
		Runner:            &wslSetupRecordingRunner{},
		Input:             strings.NewReader(""),
	}
	info := companion.Info{CompanionVersion: "1.2.3", CAFingerprint: fingerprint}
	if err := installWSLIntegration(t.Context(), info, certificate, cfg); err != nil {
		t.Fatal(err)
	}
	metadata, err := loadWSLIntegrationMetadata(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(metadata.EdgeBinary, 0644); err != nil {
		t.Fatal(err)
	}
	cfg.Runner = &wslSetupRecordingRunner{}
	if err := installWSLIntegration(t.Context(), info, certificate, cfg); err != nil {
		t.Fatal(err)
	}
	fileInfo, err := os.Stat(metadata.EdgeBinary)
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.Mode().Perm()&0111 == 0 {
		t.Fatalf("reused edge mode = %v, want executable", fileInfo.Mode().Perm())
	}
}

func TestInstallWSLIntegrationRejectsFingerprintMismatchBeforeCommands(t *testing.T) {
	certificate, _ := testCA(t)
	executable := filepath.Join(t.TempDir(), "gohere")
	if err := os.WriteFile(executable, []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}
	runner := &wslSetupRecordingRunner{}

	err := installWSLIntegration(t.Context(), companion.Info{CAFingerprint: strings.Repeat("0", 64)}, certificate, wslIntegrationSetupConfig{
		StateDir:          t.TempDir(),
		CurrentExecutable: executable,
		Runner:            runner,
	})
	if err == nil || !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Fatalf("error = %v", err)
	}
	if len(runner.commands) != 0 {
		t.Fatalf("commands = %#v, want none", runner.commands)
	}
}

func TestInstallWSLIntegrationRequiresApprovalToReplaceCA(t *testing.T) {
	certificateOne, fingerprintOne := testCA(t)
	certificateTwo, fingerprintTwo := testCA(t)
	root := t.TempDir()
	executable := filepath.Join(root, "gohere")
	if err := os.WriteFile(executable, []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}
	base := wslIntegrationSetupConfig{
		StateDir:          filepath.Join(root, "state"),
		CurrentExecutable: executable,
		Runner:            &wslSetupRecordingRunner{},
		Input:             strings.NewReader(""),
	}
	if err := installWSLIntegration(t.Context(), companion.Info{CAFingerprint: fingerprintOne}, certificateOne, base); err != nil {
		t.Fatal(err)
	}

	base.Runner = &wslSetupRecordingRunner{}
	base.Input = strings.NewReader("n\n")
	err := installWSLIntegration(t.Context(), companion.Info{CAFingerprint: fingerprintTwo}, certificateTwo, base)
	if err == nil || !strings.Contains(err.Error(), "not approved") {
		t.Fatalf("error = %v", err)
	}
	stored, err := os.ReadFile(filepath.Join(base.StateDir, wslIntegrationDirname, "windows-ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != certificateOne {
		t.Fatal("unapproved CA replacement changed the stored certificate")
	}
}

func TestEnsureOwnerInstanceIsStable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owner-instance")
	first, err := ensureOwnerInstance(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ensureOwnerInstance(path)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || len(first) != 32 {
		t.Fatalf("owner instances = %q, %q", first, second)
	}
}

func TestApplyWSLTrustEnvironmentAddsCompatibilityWithoutOverwritingUserPolicy(t *testing.T) {
	stateDir := t.TempDir()
	environment := applyWSLTrustEnvironment([]string{
		"PATH=/bin",
		"NODE_EXTRA_CA_CERTS=/custom/node-ca.pem",
		"REQUESTS_CA_BUNDLE=/custom/python-ca.pem",
	}, stateDir)
	assertEnv(t, environment, "NODE_EXTRA_CA_CERTS", "/custom/node-ca.pem")
	assertEnv(t, environment, "REQUESTS_CA_BUNDLE", "/custom/python-ca.pem")

	environment = applyWSLTrustEnvironment([]string{"PATH=/bin"}, stateDir)
	assertEnv(t, environment, "NODE_EXTRA_CA_CERTS", filepath.Join(stateDir, wslIntegrationDirname, "windows-ca.pem"))
}

func TestPromptAndSetupWSLAuthorityUsesEnvironmentNeutralCopy(t *testing.T) {
	oldInput := promptInput
	oldSetup := newWindowsCompanionControlFunc
	defer func() {
		promptInput = oldInput
		newWindowsCompanionControlFunc = oldSetup
	}()
	promptInput = strings.NewReader("n\n")
	var output strings.Builder

	err := promptAndSetupWSLAuthority(t.Context(), &output)
	if err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("error = %v", err)
	}
	want := "gohere needs one-time setup for HTTPS .localhost URLs. sudo access may be requested.\n\nContinue? [Y/n] "
	if !strings.HasPrefix(output.String(), want) {
		t.Fatalf("output = %q, want prefix %q", output.String(), want)
	}
	for _, leaked := range []string{"Windows", "companion", "router", "WSL integration"} {
		if strings.Contains(output.String(), leaked) {
			t.Fatalf("normal setup output leaks %q: %q", leaked, output.String())
		}
	}
}

func TestSetupWSLAuthorityBootstrapsWindowsFirstWithoutLocalRouterState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	certificate, fingerprint := testCA(t)
	executable := filepath.Join(t.TempDir(), "npm-gohere")
	if err := os.WriteFile(executable, []byte("linux-cli"), 0755); err != nil {
		t.Fatal(err)
	}
	info := validWindowsCompanionInfo()
	info.RouterInstalled = false
	info.RouterReady = false
	info.RouterInstanceID = ""
	info.CAFingerprint = ""
	control := &wslFirstSetupControl{
		recordingWindowsCompanionControl: recordingWindowsCompanionControl{info: info},
		certificate:                      certificate,
		fingerprint:                      fingerprint,
	}
	runner := &wslSetupRecordingRunner{}
	oldFactory := newWindowsCompanionControlFunc
	oldDetectWSL2 := detectWSL2Func
	oldExecutable := wslSetupExecutableFunc
	oldRunner := wslSetupRunner
	defer func() {
		newWindowsCompanionControlFunc = oldFactory
		detectWSL2Func = oldDetectWSL2
		wslSetupExecutableFunc = oldExecutable
		wslSetupRunner = oldRunner
	}()
	detectWSL2Func = func() bool { return true }
	newWindowsCompanionControlFunc = func(context.Context) (windowsCompanionControl, error) {
		return control, nil
	}
	wslSetupExecutableFunc = func() (string, error) { return executable, nil }
	wslSetupRunner = runner

	if err := setupWSLAuthority(t.Context()); err != nil {
		t.Fatal(err)
	}
	if control.bootstrapCalls != 1 {
		t.Fatalf("bootstrap calls = %d, want 1", control.bootstrapCalls)
	}
	stateDir := router.DefaultStateDir()
	if _, err := os.Stat(filepath.Join(stateDir, wslIntegrationDirname, "metadata.json")); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		filepath.Join(stateDir, "token"),
		filepath.Join(stateDir, "routes.json"),
		filepath.Join(stateDir, "ca", "ca.key"),
	} {
		if _, err := os.Stat(forbidden); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("WSL setup created authority state %s", forbidden)
		}
	}
}

type wslFirstSetupControl struct {
	recordingWindowsCompanionControl
	certificate    string
	fingerprint    string
	bootstrapCalls int
}

func (c *wslFirstSetupControl) Bootstrap(context.Context, bool) (string, error) {
	c.bootstrapCalls++
	c.info.RouterInstalled = true
	c.info.RouterReady = true
	c.info.RouterInstanceID = "router-after-bootstrap"
	c.info.CAFingerprint = c.fingerprint
	return "Windows authority installed.\n", nil
}

func (c *wslFirstSetupControl) CACertificate(context.Context) (string, error) {
	return c.certificate, nil
}

func testCA(t *testing.T) (string, string) {
	t.Helper()
	store := localcert.Store{StateDir: t.TempDir()}
	ca, err := store.EnsureCA()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(store.CACertPath())
	if err != nil {
		t.Fatal(err)
	}
	return string(data), ca.Fingerprint
}

type wslSetupRecordingRunner struct {
	commands [][]string
	err      error
}

func (r *wslSetupRecordingRunner) Run(_ context.Context, command string, args ...string) error {
	call := append([]string{command}, args...)
	r.commands = append(r.commands, call)
	return r.err
}
