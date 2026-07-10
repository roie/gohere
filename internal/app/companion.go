package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	localcert "github.com/roie/gohere/internal/cert"
	"github.com/roie/gohere/internal/companion"
	appconfig "github.com/roie/gohere/internal/config"
	"github.com/roie/gohere/internal/router"
	"github.com/roie/gohere/internal/setup"
)

type CompanionConfig struct {
	Version       string
	GOOS          string
	GOARCH        string
	StateDir      string
	User          string
	UserProfile   string
	AdminClient   func() (adminClient, error)
	Bootstrap     func(context.Context, bool, io.Writer) error
	StartRouter   func(context.Context) error
	Doctor        func(context.Context, io.Writer) error
	Uninstall     func(context.Context, bool, io.Writer) error
	StopRouter    func(context.Context, io.Writer) error
	HealthTimeout time.Duration
}

func ServeCompanion(ctx context.Context, in io.Reader, out io.Writer, cfg CompanionConfig) error {
	authority := newCompanionAuthority(cfg)
	return companion.Serve(ctx, in, out, authority)
}

type companionAuthority struct {
	version       string
	goos          string
	goarch        string
	stateDir      string
	user          string
	userProfile   string
	adminClient   func() (adminClient, error)
	bootstrap     func(context.Context, bool, io.Writer) error
	startRouter   func(context.Context) error
	doctor        func(context.Context, io.Writer) error
	uninstall     func(context.Context, bool, io.Writer) error
	stopRouter    func(context.Context, io.Writer) error
	healthTimeout time.Duration
}

func newCompanionAuthority(cfg CompanionConfig) *companionAuthority {
	if cfg.Version == "" {
		cfg.Version = "dev"
	}
	if cfg.GOOS == "" {
		cfg.GOOS = runtime.GOOS
	}
	if cfg.GOARCH == "" {
		cfg.GOARCH = runtime.GOARCH
	}
	if cfg.StateDir == "" {
		cfg.StateDir = router.DefaultStateDir()
	}
	if cfg.UserProfile == "" {
		cfg.UserProfile, _ = os.UserHomeDir()
	}
	if cfg.User == "" {
		if current, err := user.Current(); err == nil {
			cfg.User = current.Username
		}
	}
	if cfg.AdminClient == nil {
		cfg.AdminClient = func() (adminClient, error) { return defaultAdminClient() }
	}
	if cfg.StartRouter == nil {
		cfg.StartRouter = startInstalledRouter
	}
	if cfg.Bootstrap == nil {
		version := cfg.Version
		cfg.Bootstrap = func(ctx context.Context, enableHTTPS bool, output io.Writer) error {
			return bootstrapWindowsAuthority(ctx, enableHTTPS, output, version)
		}
	}
	if cfg.Doctor == nil {
		cfg.Doctor = Doctor
	}
	if cfg.Uninstall == nil {
		cfg.Uninstall = uninstallWindowsAuthority
	}
	if cfg.StopRouter == nil {
		cfg.StopRouter = func(ctx context.Context, output io.Writer) error {
			return ServiceStopWithConfig(ctx, output, ServiceStopConfig{})
		}
	}
	if cfg.HealthTimeout <= 0 {
		cfg.HealthTimeout = routerStartTimeout
	}
	return &companionAuthority{
		version:       cfg.Version,
		goos:          cfg.GOOS,
		goarch:        cfg.GOARCH,
		stateDir:      cfg.StateDir,
		user:          cfg.User,
		userProfile:   cfg.UserProfile,
		adminClient:   cfg.AdminClient,
		bootstrap:     cfg.Bootstrap,
		startRouter:   cfg.StartRouter,
		doctor:        cfg.Doctor,
		uninstall:     cfg.Uninstall,
		stopRouter:    cfg.StopRouter,
		healthTimeout: cfg.HealthTimeout,
	}
}

func (a *companionAuthority) Info(ctx context.Context) (companion.Info, error) {
	if err := a.requireWindows(); err != nil {
		return companion.Info{}, err
	}
	routerReady := false
	if client, err := a.adminClient(); err == nil {
		routerReady = client.Health(ctx) == nil
	}
	fingerprint, _ := localcert.Store{StateDir: a.stateDir}.Fingerprint()
	listeners := []companion.Listener{
		{Name: "http", Address: "127.0.0.1:80"},
		{Name: "admin", Address: "127.0.0.1:39399"},
	}
	if cfg, err := appconfig.Load(a.stateDir); err == nil && cfg.HTTPS {
		listeners = append(listeners, companion.Listener{Name: "https", Address: "127.0.0.1:443"})
	}
	capabilities := []string{
		"control.bootstrap",
		"control.ca-certificate",
		"control.delete-route",
		"control.doctor",
		"control.ensure-router",
		"control.health",
		"control.info",
		"control.probe-target",
		"control.ready-info",
		"control.route-statuses",
		"control.routes",
		"control.upsert-route",
		"control.uninstall",
		"control.stop-router",
	}
	sort.Strings(capabilities)
	return companion.Info{
		CompanionVersion: a.version,
		Platform:         a.goos,
		Architecture:     a.goarch,
		User:             a.user,
		UserProfile:      a.userProfile,
		StateDir:         a.stateDir,
		RouterReady:      routerReady,
		RouterInstalled:  windowsRouterInstalled(a.stateDir),
		RouterInstanceID: companionRouterInstanceID(a.stateDir),
		CAFingerprint:    fingerprint,
		Capabilities:     capabilities,
		Listeners:        listeners,
	}, nil
}

func (a *companionAuthority) ReadyInfo(ctx context.Context) (companion.Info, error) {
	info, err := a.Info(ctx)
	if err != nil || !info.RouterInstalled {
		return info, err
	}
	if windowsBinaryUpgradeNeeded(ctx, a.stateDir, a.version) {
		enableHTTPS := false
		if config, configErr := appconfig.Load(a.stateDir); configErr == nil {
			enableHTTPS = config.HTTPS
		}
		if _, err := a.Bootstrap(ctx, enableHTTPS); err != nil {
			return companion.Info{}, err
		}
	}
	if err := a.EnsureRouter(ctx); err != nil {
		return companion.Info{}, err
	}
	info, err = a.Info(ctx)
	if err != nil {
		return companion.Info{}, err
	}
	if !info.RouterReady {
		return companion.Info{}, errors.New("Windows router health succeeded but router is not ready")
	}
	return info, nil
}

func (a *companionAuthority) Bootstrap(ctx context.Context, enableHTTPS bool) (string, error) {
	if err := a.requireWindows(); err != nil {
		return "", err
	}
	var output bytes.Buffer
	if err := a.bootstrap(ctx, enableHTTPS, &output); err != nil {
		detail := strings.TrimSpace(output.String())
		if detail != "" {
			return "", fmt.Errorf("%w\n%s", err, detail)
		}
		return "", err
	}
	return output.String(), nil
}

func (a *companionAuthority) CACertificate(context.Context) (string, error) {
	if err := a.requireWindows(); err != nil {
		return "", err
	}
	store := localcert.Store{StateDir: a.stateDir}
	if _, err := store.Fingerprint(); err != nil {
		return "", fmt.Errorf("Windows certificate authority is unavailable: %w", err)
	}
	certificate, err := os.ReadFile(store.CACertPath())
	if err != nil {
		return "", err
	}
	return string(certificate), nil
}

func (a *companionAuthority) EnsureRouter(ctx context.Context) error {
	if err := a.requireWindows(); err != nil {
		return err
	}
	client, err := a.adminClient()
	if err != nil {
		return fmt.Errorf("Windows router state is not installed: %w", err)
	}
	if client.Health(ctx) == nil {
		return nil
	}
	if err := a.startRouter(ctx); err != nil {
		return fmt.Errorf("could not start Windows router: %w", err)
	}
	if err := waitForRouterHealth(ctx, client.Health, a.healthTimeout); err != nil {
		return fmt.Errorf("Windows router did not become ready: %w", err)
	}
	return nil
}

func (a *companionAuthority) Health(ctx context.Context) error {
	client, err := a.client()
	if err != nil {
		return err
	}
	return client.Health(ctx)
}

func (a *companionAuthority) Routes(ctx context.Context) ([]router.Route, error) {
	client, err := a.client()
	if err != nil {
		return nil, err
	}
	return client.Routes(ctx)
}

func (a *companionAuthority) RouteStatuses(ctx context.Context) ([]router.RouteStatus, error) {
	client, err := a.client()
	if err != nil {
		return nil, err
	}
	if statusClient, ok := client.(routeStatusClient); ok {
		return statusClient.RouteStatuses(ctx)
	}
	statuses, err := adminRouteStatuses(ctx, client)
	if err != nil {
		return nil, err
	}
	out := make([]router.RouteStatus, 0, len(statuses))
	for _, status := range statuses {
		out = append(out, router.RouteStatus{Route: status.Route, Status: string(status.Status)})
	}
	return out, nil
}

func (a *companionAuthority) Doctor(ctx context.Context) (string, error) {
	if err := a.requireWindows(); err != nil {
		return "", err
	}
	var output bytes.Buffer
	if err := a.doctor(ctx, &output); err != nil {
		return "", err
	}
	return output.String(), nil
}

func (a *companionAuthority) Uninstall(ctx context.Context, removeState bool) (string, error) {
	if err := a.requireWindows(); err != nil {
		return "", err
	}
	var output bytes.Buffer
	if err := a.uninstall(ctx, removeState, &output); err != nil {
		return "", err
	}
	return output.String(), nil
}

func (a *companionAuthority) StopRouter(ctx context.Context) (string, error) {
	if err := a.requireWindows(); err != nil {
		return "", err
	}
	var output bytes.Buffer
	if err := a.stopRouter(ctx, &output); err != nil {
		return "", err
	}
	return output.String(), nil
}

func (a *companionAuthority) UpsertRoute(ctx context.Context, route router.Route) error {
	client, err := a.client()
	if err != nil {
		return err
	}
	return client.UpsertRoute(ctx, route)
}

func (a *companionAuthority) DeleteRoute(ctx context.Context, host string) error {
	client, err := a.client()
	if err != nil {
		return err
	}
	return client.DeleteRoute(ctx, host)
}

func (a *companionAuthority) ProbeTarget(ctx context.Context, target string) (bool, error) {
	client, err := a.client()
	if err != nil {
		return false, err
	}
	probeClient, ok := client.(bridgeProbeClient)
	if !ok {
		return false, errors.New("Windows router does not support target probes")
	}
	return probeClient.ProbeTarget(ctx, target)
}

func (a *companionAuthority) client() (adminClient, error) {
	if err := a.requireWindows(); err != nil {
		return nil, err
	}
	client, err := a.adminClient()
	if err != nil {
		return nil, fmt.Errorf("could not open Windows router control: %w", err)
	}
	return client, nil
}

func (a *companionAuthority) requireWindows() error {
	if a == nil || a.goos != "windows" {
		goos := "unknown"
		if a != nil && strings.TrimSpace(a.goos) != "" {
			goos = a.goos
		}
		return fmt.Errorf("companion mode requires Windows; running on %s", goos)
	}
	return nil
}

func companionRouterInstanceID(stateDir string) string {
	if data, err := os.ReadFile(filepath.Join(stateDir, router.RouterInstanceFilename)); err == nil {
		instanceID := strings.TrimSpace(string(data))
		if instanceID != "" {
			return instanceID
		}
	}
	data, err := os.ReadFile(filepath.Join(stateDir, "router.pid"))
	if err != nil {
		return ""
	}
	pid := strings.TrimSpace(string(data))
	if pid == "" {
		return ""
	}
	return "legacy-pid:" + pid
}

func windowsRouterInstalled(stateDir string) bool {
	return exists(filepath.Join(stateDir, "bin", "gohere.exe")) &&
		exists(filepath.Join(stateDir, "token"))
}

func bootstrapWindowsAuthority(ctx context.Context, enableHTTPS bool, output io.Writer, companionVersion string) error {
	if output == nil {
		output = io.Discard
	}
	stateDir := router.DefaultStateDir()
	client, clientErr := defaultAdminClient()
	routerHealthy := clientErr == nil && client.Health(ctx) == nil
	config, configErr := appconfig.Load(stateDir)
	needsInstanceUpgrade := !exists(filepath.Join(stateDir, router.RouterInstanceFilename))
	needsBinaryUpgrade := windowsBinaryUpgradeNeeded(ctx, stateDir, companionVersion)
	if routerHealthy && !needsInstanceUpgrade && !needsBinaryUpgrade && (!enableHTTPS || configErr == nil && config.HTTPS) {
		return nil
	}
	if routerHealthy {
		fmt.Fprintln(output, "Refreshing the Windows router...")
		if err := ServiceStopWithConfig(ctx, output, ServiceStopConfig{StateDir: stateDir}); err != nil {
			return err
		}
	}
	if err := setup.Windows(ctx, setup.Config{
		StateDir:      stateDir,
		HTTPS:         enableHTTPS,
		Progress:      output,
		Stderr:        output,
		CommandStdout: output,
		CommandStderr: output,
	}); err != nil {
		return err
	}
	client, err := defaultAdminClient()
	if err != nil {
		return err
	}
	if err := waitForRouterHealth(ctx, client.Health, routerStartTimeout); err != nil {
		return fmt.Errorf("Windows router did not become ready after setup: %w", err)
	}
	return nil
}

type releaseVersion [3]uint64

func windowsBinaryUpgradeNeeded(ctx context.Context, stateDir, companionVersion string) bool {
	if _, ok := parseReleaseVersion(companionVersion); !ok {
		return false
	}
	stableBinary := filepath.Join(stateDir, "bin", "gohere.exe")
	versionCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(versionCtx, stableBinary, "--version").Output()
	if err != nil {
		return false
	}
	return shouldUpgradeWindowsBinary(companionVersion, string(output))
}

func shouldUpgradeWindowsBinary(companionVersion, installedVersion string) bool {
	current, currentOK := parseReleaseVersion(companionVersion)
	installed, installedOK := parseReleaseVersion(installedVersion)
	return currentOK && (!installedOK || compareReleaseVersions(current, installed) > 0)
}

func parseReleaseVersion(value string) (releaseVersion, bool) {
	value = strings.TrimSpace(value)
	value = strings.TrimSpace(strings.TrimPrefix(value, "gohere"))
	value = strings.TrimPrefix(value, "v")
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return releaseVersion{}, false
	}
	var parsed releaseVersion
	for index, part := range parts {
		if part == "" {
			return releaseVersion{}, false
		}
		component, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return releaseVersion{}, false
		}
		parsed[index] = component
	}
	return parsed, true
}

func compareReleaseVersions(left, right releaseVersion) int {
	for index := range left {
		if left[index] < right[index] {
			return -1
		}
		if left[index] > right[index] {
			return 1
		}
	}
	return 0
}

func uninstallWindowsAuthority(ctx context.Context, removeState bool, output io.Writer) error {
	return UninstallWithConfig(ctx, output, UninstallConfig{
		CommandRunner: capturedCommandRunner{output: output},
		RemoveState:   &removeState,
	})
}

type capturedCommandRunner struct {
	output io.Writer
}

func (r capturedCommandRunner) Run(ctx context.Context, command string, args ...string) error {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Stdout = r.output
	cmd.Stderr = r.output
	return cmd.Run()
}

var _ companion.Authority = (*companionAuthority)(nil)
