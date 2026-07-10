package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/roie/gohere/internal/companion"
	"github.com/roie/gohere/internal/router"
)

func TestResolveWindowsCompanionStartsInstalledRouterAndRefreshesHandshake(t *testing.T) {
	control := &recordingWindowsCompanionControl{info: validWindowsCompanionInfo()}
	control.info.RouterReady = false
	control.info.RouterInstanceID = ""
	restore := stubWindowsCompanionFactory(t, control, nil)
	defer restore()

	resolved, err := resolveWindowsCompanion(t.Context(), "control.route-statuses")
	if err != nil {
		t.Fatal(err)
	}
	if control.readyInfoCalls != 1 || control.ensureCalls != 0 || control.healthCalls != 0 || control.infoCalls != 0 {
		t.Fatalf("calls = ready info %d, info %d, ensure %d, health %d", control.readyInfoCalls, control.infoCalls, control.ensureCalls, control.healthCalls)
	}
	if !resolved.Info.RouterReady || resolved.Info.RouterInstanceID != "router-after-ensure" {
		t.Fatalf("resolved info = %#v", resolved.Info)
	}
}

func TestResolveWindowsCompanionNeverFallsBackWhenWindowsIsNotInstalled(t *testing.T) {
	control := &recordingWindowsCompanionControl{info: validWindowsCompanionInfo()}
	control.info.RouterInstalled = false
	control.info.RouterReady = false
	control.info.RouterInstanceID = ""
	restore := stubWindowsCompanionFactory(t, control, nil)
	defer restore()

	_, err := resolveWindowsCompanion(t.Context())
	if err == nil || !strings.Contains(err.Error(), "run gohere setup from WSL") ||
		!strings.Contains(err.Error(), "No WSL router was started") {
		t.Fatalf("error = %v", err)
	}
	if control.ensureCalls != 0 {
		t.Fatalf("ensure calls = %d, want 0", control.ensureCalls)
	}
}

func TestOpenWindowsCompanionAllowsBootstrapBeforeInstall(t *testing.T) {
	control := &recordingWindowsCompanionControl{info: validWindowsCompanionInfo()}
	control.info.RouterInstalled = false
	control.info.RouterReady = false
	control.info.RouterInstanceID = ""
	restore := stubWindowsCompanionFactory(t, control, nil)
	defer restore()

	resolved, err := openWindowsCompanion(t.Context(), "control.bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Info.RouterInstalled || control.ensureCalls != 0 || control.healthCalls != 0 {
		t.Fatalf("resolved = %#v, calls = ensure %d health %d", resolved, control.ensureCalls, control.healthCalls)
	}
}

func TestResolveWindowsCompanionRequiresCommandCapability(t *testing.T) {
	control := &recordingWindowsCompanionControl{info: validWindowsCompanionInfo()}
	restore := stubWindowsCompanionFactory(t, control, nil)
	defer restore()

	_, err := resolveWindowsCompanion(t.Context(), "control.not-real")
	if err == nil || !strings.Contains(err.Error(), "does not advertise control.not-real") {
		t.Fatalf("error = %v", err)
	}
}

func TestResolveWindowsCompanionPreservesFactoryFailureAndNoFallbackInvariant(t *testing.T) {
	restore := stubWindowsCompanionFactory(t, nil, errors.New("npm companion missing"))
	defer restore()

	_, err := resolveWindowsCompanion(t.Context())
	if err == nil || !strings.Contains(err.Error(), "npm companion missing") ||
		!strings.Contains(err.Error(), "No WSL router was started") {
		t.Fatalf("error = %v", err)
	}
}

func TestOpenWindowsCompanionRejectsWSL1BeforeLaunchingHelper(t *testing.T) {
	oldDetectWSL2 := detectWSL2Func
	oldFactory := newWindowsCompanionControlFunc
	defer func() {
		detectWSL2Func = oldDetectWSL2
		newWindowsCompanionControlFunc = oldFactory
	}()
	detectWSL2Func = func() bool { return false }
	newWindowsCompanionControlFunc = func(context.Context) (windowsCompanionControl, error) {
		t.Fatal("Windows helper must not launch under WSL1")
		return nil, nil
	}

	_, err := openWindowsCompanion(t.Context())
	if err == nil || !strings.Contains(err.Error(), "requires WSL2") ||
		!strings.Contains(err.Error(), "No WSL router was started") {
		t.Fatalf("error = %v", err)
	}
}

func TestNewWindowsCompanionControlLetsGoInstallReuseDiscoverableWindowsBinary(t *testing.T) {
	oldGetenv := companionGetenvFunc
	oldExecutable := companionExecutableFunc
	oldBundled := companionBundledFunc
	oldLookPath := companionLookPathFunc
	oldStage := companionStageFunc
	defer func() {
		companionGetenvFunc = oldGetenv
		companionExecutableFunc = oldExecutable
		companionBundledFunc = oldBundled
		companionLookPathFunc = oldLookPath
		companionStageFunc = oldStage
	}()
	companionGetenvFunc = func(string) string { return "" }
	companionExecutableFunc = func() (string, error) { return "/home/alice/go/bin/gohere", nil }
	companionBundledFunc = func(string, string) (string, error) {
		return "", errors.New("install gohere through npm")
	}
	companionLookPathFunc = func(name string) (string, error) {
		if name != "gohere.exe" {
			t.Fatalf("look path name = %q", name)
		}
		return "/mnt/c/Users/Alice/.gohere/bin/gohere.exe", nil
	}
	companionStageFunc = func(context.Context, string) (string, error) {
		t.Fatal("discoverable stable Windows binary must not be restaged")
		return "", nil
	}

	control, err := newWindowsCompanionControl(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if control.Executable() != "/mnt/c/Users/Alice/.gohere/bin/gohere.exe" {
		t.Fatalf("executable = %q", control.Executable())
	}
}

func TestNewWindowsCompanionControlExplainsNPMRequirementWhenNothingIsDiscoverable(t *testing.T) {
	oldGetenv := companionGetenvFunc
	oldExecutable := companionExecutableFunc
	oldBundled := companionBundledFunc
	oldLookPath := companionLookPathFunc
	defer func() {
		companionGetenvFunc = oldGetenv
		companionExecutableFunc = oldExecutable
		companionBundledFunc = oldBundled
		companionLookPathFunc = oldLookPath
	}()
	companionGetenvFunc = func(string) string { return "" }
	companionExecutableFunc = func() (string, error) { return "/home/alice/go/bin/gohere", nil }
	companionBundledFunc = func(string, string) (string, error) {
		return "", errors.New("install gohere through npm")
	}
	companionLookPathFunc = func(string) (string, error) { return "", errors.New("not found") }

	_, err := newWindowsCompanionControl(t.Context())
	if err == nil || !strings.Contains(err.Error(), "install gohere through npm") ||
		!strings.Contains(err.Error(), "no compatible Windows gohere.exe") {
		t.Fatalf("error = %v", err)
	}
}

func validWindowsCompanionInfo() companion.Info {
	return companion.Info{
		CompanionVersion: "1.2.3",
		Platform:         "windows",
		Architecture:     "amd64",
		User:             `DESKTOP\Alice`,
		UserProfile:      `C:\Users\Alice`,
		StateDir:         `C:\Users\Alice\.gohere`,
		RouterInstalled:  true,
		RouterReady:      true,
		RouterInstanceID: "router-1",
		Capabilities: []string{
			"control.bootstrap",
			"control.ca-certificate",
			"control.doctor",
			"control.ensure-router",
			"control.health",
			"control.info",
			"control.ready-info",
			"control.route-statuses",
			"control.uninstall",
			"control.stop-router",
		},
	}
}

func stubWindowsCompanionFactory(t *testing.T, control windowsCompanionControl, err error) func() {
	t.Helper()
	old := newWindowsCompanionControlFunc
	oldDetectWSL2 := detectWSL2Func
	detectWSL2Func = func() bool { return true }
	newWindowsCompanionControlFunc = func(context.Context) (windowsCompanionControl, error) {
		return control, err
	}
	return func() {
		newWindowsCompanionControlFunc = old
		detectWSL2Func = oldDetectWSL2
	}
}

type recordingWindowsCompanionControl struct {
	info           companion.Info
	infoCalls      int
	ensureCalls    int
	healthCalls    int
	readyInfoCalls int
}

func (*recordingWindowsCompanionControl) Executable() string { return `C:\Temp\gohere.exe` }

func (c *recordingWindowsCompanionControl) Info(context.Context) (companion.Info, error) {
	c.infoCalls++
	return c.info, nil
}

func (c *recordingWindowsCompanionControl) ReadyInfo(context.Context) (companion.Info, error) {
	c.readyInfoCalls++
	if c.info.RouterInstalled && !c.info.RouterReady {
		c.info.RouterReady = true
		c.info.RouterInstanceID = "router-after-ensure"
	}
	return c.info, nil
}

func (c *recordingWindowsCompanionControl) Bootstrap(context.Context, bool) (string, error) {
	return "", nil
}

func (c *recordingWindowsCompanionControl) CACertificate(context.Context) (string, error) {
	return "certificate", nil
}

func (c *recordingWindowsCompanionControl) EnsureRouter(context.Context) error {
	c.ensureCalls++
	c.info.RouterReady = true
	c.info.RouterInstanceID = "router-after-ensure"
	return nil
}

func (c *recordingWindowsCompanionControl) Health(context.Context) error {
	c.healthCalls++
	return nil
}

func (*recordingWindowsCompanionControl) Routes(context.Context) ([]router.Route, error) {
	return nil, nil
}

func (*recordingWindowsCompanionControl) RouteStatuses(context.Context) ([]router.RouteStatus, error) {
	return nil, nil
}

func (*recordingWindowsCompanionControl) Doctor(context.Context) (string, error) { return "", nil }

func (*recordingWindowsCompanionControl) Uninstall(context.Context, bool) (string, error) {
	return "", nil
}

func (*recordingWindowsCompanionControl) StopRouter(context.Context) (string, error) {
	return "", nil
}

func (*recordingWindowsCompanionControl) UpsertRoute(context.Context, router.Route) error { return nil }

func (*recordingWindowsCompanionControl) DeleteRoute(context.Context, string) error { return nil }

func (*recordingWindowsCompanionControl) ProbeTarget(context.Context, string) (bool, error) {
	return true, nil
}
