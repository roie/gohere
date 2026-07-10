package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/roie/gohere/internal/companion"
	"github.com/roie/gohere/internal/router"
)

const windowsCompanionOverrideEnv = "GOHERE_WINDOWS_COMPANION"

type windowsCompanionControl interface {
	Executable() string
	Info(context.Context) (companion.Info, error)
	ReadyInfo(context.Context) (companion.Info, error)
	Bootstrap(context.Context, bool) (string, error)
	CACertificate(context.Context) (string, error)
	EnsureRouter(context.Context) error
	Health(context.Context) error
	Routes(context.Context) ([]router.Route, error)
	RouteStatuses(context.Context) ([]router.RouteStatus, error)
	Doctor(context.Context) (string, error)
	Uninstall(context.Context, bool) (string, error)
	StopRouter(context.Context) (string, error)
	UpsertRoute(context.Context, router.Route) error
	DeleteRoute(context.Context, string) error
	ProbeTarget(context.Context, string) (bool, error)
}

type resolvedWindowsCompanion struct {
	Control    windowsCompanionControl
	Info       companion.Info
	Executable string
}

var (
	companionExecutableFunc = os.Executable
	companionGetenvFunc     = os.Getenv
	companionBundledFunc    = companion.BundledWindowsBinary
	companionLookPathFunc   = exec.LookPath
	companionStageFunc      = func(ctx context.Context, source string) (string, error) {
		return companion.StageWindowsBinary(ctx, source, companion.ExecOutputRunner{})
	}
	companionProcessRunner         companion.ProcessRunner = companion.ExecRunner{}
	newWindowsCompanionControlFunc                         = newWindowsCompanionControl
)

func newWindowsCompanionControl(ctx context.Context) (windowsCompanionControl, error) {
	binary := strings.TrimSpace(companionGetenvFunc(windowsCompanionOverrideEnv))
	if binary == "" {
		currentExecutable, err := companionExecutableFunc()
		if err != nil {
			return nil, err
		}
		bundled, err := companionBundledFunc(currentExecutable, runtime.GOARCH)
		if err != nil {
			discovered, discoverErr := companionLookPathFunc("gohere.exe")
			if discoverErr != nil {
				return nil, fmt.Errorf("%w; no compatible Windows gohere.exe was found on PATH", err)
			}
			binary = discovered
		} else {
			binary, err = companionStageFunc(ctx, bundled)
			if err != nil {
				return nil, err
			}
		}
	}
	return &companion.Client{Binary: binary, Runner: companionProcessRunner, Diagnostics: os.Stderr}, nil
}

func resolveWindowsCompanion(ctx context.Context, requiredCapabilities ...string) (resolvedWindowsCompanion, error) {
	if !detectWSL2Func() {
		return resolvedWindowsCompanion{}, windowsCompanionUnavailableError(errors.New("Windows host integration requires WSL2; WSL1 is not supported"))
	}
	control, err := newWindowsCompanionControlFunc(ctx)
	if err != nil {
		return resolvedWindowsCompanion{}, windowsCompanionUnavailableError(err)
	}
	info, err := control.ReadyInfo(ctx)
	if err != nil {
		return resolvedWindowsCompanion{}, windowsCompanionUnavailableError(err)
	}
	if err := validateWindowsCompanionInfo(info, requiredCapabilities...); err != nil {
		return resolvedWindowsCompanion{}, windowsCompanionUnavailableError(err)
	}
	if !info.RouterInstalled {
		return resolvedWindowsCompanion{}, windowsCompanionUnavailableError(errors.New("Windows router is not installed; run gohere setup from WSL"))
	}
	if !info.RouterReady {
		return resolvedWindowsCompanion{}, windowsCompanionUnavailableError(errors.New("Windows router is not ready"))
	}
	return resolvedWindowsCompanion{Control: control, Info: info, Executable: control.Executable()}, nil
}

func openWindowsCompanion(ctx context.Context, requiredCapabilities ...string) (resolvedWindowsCompanion, error) {
	if !detectWSL2Func() {
		return resolvedWindowsCompanion{}, windowsCompanionUnavailableError(errors.New("Windows host integration requires WSL2; WSL1 is not supported"))
	}
	control, err := newWindowsCompanionControlFunc(ctx)
	if err != nil {
		return resolvedWindowsCompanion{}, windowsCompanionUnavailableError(err)
	}
	info, err := control.Info(ctx)
	if err != nil {
		return resolvedWindowsCompanion{}, windowsCompanionUnavailableError(err)
	}
	if err := validateWindowsCompanionInfo(info, requiredCapabilities...); err != nil {
		return resolvedWindowsCompanion{}, windowsCompanionUnavailableError(err)
	}
	return resolvedWindowsCompanion{Control: control, Info: info, Executable: control.Executable()}, nil
}

func validateWindowsCompanionInfo(info companion.Info, requiredCapabilities ...string) error {
	if info.Platform != "windows" {
		return fmt.Errorf("companion reported platform %q", info.Platform)
	}
	if strings.TrimSpace(info.CompanionVersion) == "" {
		return errors.New("companion did not report its version")
	}
	if strings.TrimSpace(info.Architecture) == "" {
		return errors.New("companion did not report its architecture")
	}
	if strings.TrimSpace(info.UserProfile) == "" {
		return errors.New("companion did not report the Windows user profile")
	}
	if strings.TrimSpace(info.StateDir) == "" {
		return errors.New("companion did not report the Windows state directory")
	}
	if info.RouterReady && strings.TrimSpace(info.RouterInstanceID) == "" {
		return errors.New("ready companion did not report a router instance")
	}
	required := append([]string{"control.info", "control.ready-info", "control.ensure-router", "control.health"}, requiredCapabilities...)
	for _, capability := range required {
		if !hasCompanionCapability(info, capability) {
			return fmt.Errorf("companion does not advertise %s", capability)
		}
	}
	return nil
}

func hasCompanionCapability(info companion.Info, capability string) bool {
	for _, candidate := range info.Capabilities {
		if candidate == capability {
			return true
		}
	}
	return false
}

func windowsCompanionUnavailableError(err error) error {
	message := "gohere could not use the Windows authority from WSL.\n\nNo WSL router was started."
	if err == nil {
		return errors.New(message)
	}
	return fmt.Errorf("%s\n\nDetails: %w", message, err)
}

var _ windowsCompanionControl = (*companion.Client)(nil)
