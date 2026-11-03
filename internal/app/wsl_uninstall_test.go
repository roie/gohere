package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/roie/gohere/internal/companion"
	"github.com/roie/gohere/internal/router"
)

func TestUninstallWSLIntegrationRemovesOnlyLocalEdgeWhenCompanionUnavailable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	certificate, fingerprint := testCA(t)
	stateDir := router.DefaultStateDir()
	integrationDir := filepath.Join(stateDir, wslIntegrationDirname)
	if err := os.MkdirAll(integrationDir, 0700); err != nil {
		t.Fatal(err)
	}
	metadata := wslIntegrationMetadata{
		ProtocolVersion: companion.ProtocolVersion,
		OwnerInstance:   "owner-1",
		CAFingerprint:   fingerprint,
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(integrationDir, "metadata.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	trustedPath := filepath.Join(t.TempDir(), "gohere-local-ca.crt")
	if err := os.WriteFile(trustedPath, []byte(certificate), 0644); err != nil {
		t.Fatal(err)
	}

	oldFactory := newWindowsCompanionControlFunc
	oldDetectWSL2 := detectWSL2Func
	oldStopEdge := wslStopEdgeFunc
	oldRunner := wslUntrustCARunner
	oldTrustedPath := wslTrustedCAPathFunc
	oldPromptInput := promptInput
	defer func() {
		newWindowsCompanionControlFunc = oldFactory
		detectWSL2Func = oldDetectWSL2
		wslStopEdgeFunc = oldStopEdge
		wslUntrustCARunner = oldRunner
		wslTrustedCAPathFunc = oldTrustedPath
		promptInput = oldPromptInput
	}()
	detectWSL2Func = func() bool { return true }
	newWindowsCompanionControlFunc = func(context.Context) (windowsCompanionControl, error) {
		return nil, errors.New("interop disabled")
	}
	stopped := false
	wslStopEdgeFunc = func(path string) error {
		stopped = path == integrationDir
		return nil
	}
	runner := &wslSetupRecordingRunner{}
	wslUntrustCARunner = runner
	wslTrustedCAPathFunc = func() string { return trustedPath }
	promptInput = strings.NewReader("")
	var output strings.Builder

	if err := uninstallWSLIntegration(t.Context(), &output); err != nil {
		t.Fatal(err)
	}
	if !stopped {
		t.Fatal("WSL edge was not stopped")
	}
	if _, err := os.Stat(integrationDir); !os.IsNotExist(err) {
		t.Fatalf("integration dir remains: %v", err)
	}
	if !strings.Contains(output.String(), "Windows gohere authority kept") ||
		!strings.Contains(output.String(), "interop disabled") {
		t.Fatalf("output = %q", output.String())
	}
	wantCommands := [][]string{
		{"sudo", "rm", "-f", "/usr/local/share/ca-certificates/gohere-local-ca.crt"},
		{"sudo", "update-ca-certificates"},
	}
	if len(runner.commands) != len(wantCommands) {
		t.Fatalf("commands = %#v", runner.commands)
	}
}

func TestUninstallWSLIntegrationSkipsWindowsRemovalWhenRouterNotInstalled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	integrationDir := filepath.Join(router.DefaultStateDir(), wslIntegrationDirname)
	if err := os.MkdirAll(integrationDir, 0700); err != nil {
		t.Fatal(err)
	}

	oldFactory := newWindowsCompanionControlFunc
	oldDetectWSL2 := detectWSL2Func
	oldStopEdge := wslStopEdgeFunc
	oldPromptInput := promptInput
	defer func() {
		newWindowsCompanionControlFunc = oldFactory
		detectWSL2Func = oldDetectWSL2
		wslStopEdgeFunc = oldStopEdge
		promptInput = oldPromptInput
	}()
	detectWSL2Func = func() bool { return true }
	newWindowsCompanionControlFunc = func(context.Context) (windowsCompanionControl, error) {
		return &testWindowsCompanionControl{installed: false}, nil
	}
	wslStopEdgeFunc = func(string) error { return nil }
	promptInput = strings.NewReader("y\ny\n")
	var output strings.Builder

	if err := uninstallWSLIntegration(t.Context(), &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "Remove the shared Windows gohere router too?") {
		t.Fatalf("offered to remove a Windows router that is not installed: %q", output.String())
	}
	if !strings.Contains(output.String(), "No complete Windows gohere router installation was found.") {
		t.Fatalf("output = %q", output.String())
	}
	if _, err := os.Stat(integrationDir); !os.IsNotExist(err) {
		t.Fatalf("integration dir remains: %v", err)
	}
}

type routesFailingAdminClient struct{ fakeAdminClient }

func (routesFailingAdminClient) Routes(context.Context) ([]router.Route, error) {
	return nil, errors.New("connection refused")
}

func TestUninstallWSLIntegrationFinishesLocalCleanupWhenWindowsRoutesUnavailable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	integrationDir := filepath.Join(router.DefaultStateDir(), wslIntegrationDirname)
	if err := os.MkdirAll(integrationDir, 0700); err != nil {
		t.Fatal(err)
	}

	oldFactory := newWindowsCompanionControlFunc
	oldDetectWSL2 := detectWSL2Func
	oldStopEdge := wslStopEdgeFunc
	oldPromptInput := promptInput
	defer func() {
		newWindowsCompanionControlFunc = oldFactory
		detectWSL2Func = oldDetectWSL2
		wslStopEdgeFunc = oldStopEdge
		promptInput = oldPromptInput
	}()
	detectWSL2Func = func() bool { return true }
	newWindowsCompanionControlFunc = func(context.Context) (windowsCompanionControl, error) {
		return &testWindowsCompanionControl{installed: true, admin: routesFailingAdminClient{}}, nil
	}
	stopped := false
	wslStopEdgeFunc = func(string) error { stopped = true; return nil }
	promptInput = strings.NewReader("y\n")
	var output strings.Builder

	err := uninstallWSLIntegration(t.Context(), &output)

	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("err = %v, want Windows route listing failure", err)
	}
	if !stopped {
		t.Fatal("WSL edge was not stopped")
	}
	if _, statErr := os.Stat(integrationDir); !os.IsNotExist(statErr) {
		t.Fatalf("integration dir remains: %v", statErr)
	}
	if !strings.Contains(output.String(), "Windows authority removal failed; WSL cleanup completed.") {
		t.Fatalf("output = %q", output.String())
	}
}
