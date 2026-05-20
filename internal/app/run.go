package app

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/roie/gohere/internal/admin"
	"github.com/roie/gohere/internal/bridge"
	"github.com/roie/gohere/internal/cli"
	"github.com/roie/gohere/internal/lifecycle"
	"github.com/roie/gohere/internal/opener"
	"github.com/roie/gohere/internal/project"
	"github.com/roie/gohere/internal/router"
	"github.com/roie/gohere/internal/runner"
	"github.com/roie/gohere/internal/setup"
	"github.com/roie/gohere/internal/staticserver"
)

var (
	promptInput = io.Reader(os.Stdin)
	setupFunc   = func(ctx context.Context) error {
		return setupForGOOS(ctx, runtime.GOOS)
	}
	setupLinuxFunc                                       = setup.Linux
	setupWindowsFunc                                     = setup.Windows
	startInstalledRouterFunc                             = startInstalledRouter
	defaultAdminClientFunc   func() (adminClient, error) = func() (adminClient, error) {
		return defaultAdminClient()
	}
	startRunnerFunc           = runner.Start
	detectWSLFunc             = bridge.DetectWSL
	routerHealthFunc          = func(ctx context.Context) error { return admin.NewClient("http://127.0.0.1:39399", "").Health(ctx) }
	windowsRouterHealthFunc   = func(ctx context.Context) error { return admin.NewClient(windowsAdminBaseURL, "").Health(ctx) }
	discoverWindowsTokenFunc  = bridge.DiscoverWindowsToken
	windowsStableBinaryExists = bridge.WindowsStableBinaryExists
	newWindowsAdminClientFunc = func(token string) bridgeAdminClient { return admin.NewClient(windowsAdminBaseURL, token) }
	currentWSLIPFunc          = bridge.CurrentWSLIP
	probeBridgeFunc           = func(ctx context.Context, client bridgeProbeClient, wslIP string) (bool, string, error) {
		return bridge.ProbeBridge(ctx, client, wslIP)
	}
	openBrowserFunc = func(ctx context.Context, url string) error {
		return opener.Open(ctx, runtime.GOOS, detectWSLFunc(), url)
	}
)

type adminClient interface {
	Health(context.Context) error
	Routes(context.Context) ([]router.Route, error)
	UpsertRoute(context.Context, router.Route) error
	DeleteRoute(context.Context, string) error
}

type bridgeProbeClient interface {
	ProbeTarget(context.Context, string) (bool, error)
}

type bridgeAdminClient interface {
	adminClient
	bridgeProbeClient
}

const (
	windowsAdminBaseURL = "http://127.0.0.1:39399"
	windowsUsersRoot    = "/mnt/c/Users"
)

type runRouter struct {
	Client          adminClient
	RouteTargetHost string
	ChildHost       string
	RouteSource     string
	OwnerEnv        string
	RouterLabel     string
	Bridge          bool
}

type RunPlan struct {
	Command             []string
	Env                 []string
	Port                int
	Host                string
	Name                string
	CWD                 string
	ProjectRoot         string
	Static              bool
	URLPath             string
	RequireDetectedPort bool
	RouteTargetHost     string
	RouteSource         string
	OwnerEnv            string
	RouterLabel         string
	StaticBindHost      string
}

func PrepareRun(cmd cli.Command, cwd string) (RunPlan, error) {
	port := cmd.TargetPort
	if port == 0 {
		var err error
		port, err = runner.ChooseFreePort()
		if err != nil {
			return RunPlan{}, err
		}
	}

	env := runner.ChildEnv(os.Environ(), port)
	if cmd.Kind == cli.CommandRaw {
		host := project.NormalizeHostnameName(filepath.Base(cwd)) + ".localhost"
		return RunPlan{
			Command:             append([]string(nil), cmd.Raw...),
			Env:                 env,
			Port:                port,
			Host:                host,
			Name:                strings.TrimSuffix(host, ".localhost"),
			CWD:                 cwd,
			RequireDetectedPort: cmd.TargetPort == 0,
		}, nil
	}

	if isFileTarget(cmd) {
		return prepareStaticFileTarget(cmd, cwd, port)
	}
	currentPackagePath := filepath.Join(cwd, "package.json")
	_, currentPackageErr := os.Stat(currentPackagePath)
	hasCurrentPackage := currentPackageErr == nil
	if currentPackageErr != nil && !errors.Is(currentPackageErr, os.ErrNotExist) {
		return RunPlan{}, currentPackageErr
	}
	if cmd.Script == "dev" && staticserver.IsStaticProject(cwd) && !hasCurrentPackage {
		host := project.NormalizeHostnameName(filepath.Base(cwd)) + ".localhost"
		return RunPlan{Port: port, Host: host, Name: strings.TrimSuffix(host, ".localhost"), CWD: cwd, Static: true}, nil
	}

	packagePath, found, err := project.FindNearestPackageJSON(cwd)
	if err != nil {
		return RunPlan{}, err
	}
	if !found {
		if cmd.Script != "" && cmd.Script != "dev" && staticserver.IsStaticProject(cwd) {
			return RunPlan{}, fmt.Errorf("Static files need a file extension: %s", cmd.Script)
		}
		if staticserver.IsStaticProject(cwd) {
			host := project.NormalizeHostnameName(filepath.Base(cwd)) + ".localhost"
			return RunPlan{Port: port, Host: host, Name: strings.TrimSuffix(host, ".localhost"), CWD: cwd, Static: true}, nil
		}
		return RunPlan{}, errors.New("No package.json or index.html found; use gohere -- <command>.")
	}

	pkg, err := project.ReadPackageJSON(packagePath)
	if err != nil {
		return RunPlan{}, err
	}
	scriptCommand, ok := pkg.Script(cmd.Script)
	if !ok {
		return RunPlan{}, missingScriptError(cmd.Script, pkg.AvailableScripts())
	}

	pm, _, err := project.DetectPackageManager(projectDir(packagePath))
	if err != nil {
		return RunPlan{}, err
	}
	injected := runner.InjectPortArgs(scriptCommand, port, cmd.PortFlag)
	command := runner.BuildScriptCommand(pm, cmd.Script, injected)
	host, err := project.HostnameForProject(cwd)
	if err != nil {
		return RunPlan{}, err
	}
	return RunPlan{Command: command, Env: env, Port: port, Host: host, Name: strings.TrimSuffix(host, ".localhost"), CWD: cwd, ProjectRoot: projectDir(packagePath)}, nil
}

func Run(ctx context.Context, cmd cli.Command, cwd string, stdout, stderr io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	plan, err := PrepareRun(cmd, cwd)
	if err != nil {
		return err
	}

	runRouter, err := resolveRunRouter(ctx, stderr)
	if err != nil {
		return err
	}
	adminClient := runRouter.Client
	applyRunRouter(&plan, runRouter)

	if plan.Static {
		staticServer, err := staticserver.StartWithHost(ctx, plan.CWD, plan.Port, plan.StaticBindHost)
		if err != nil {
			return err
		}
		defer staticServer.Close()
		cleanup, err := registerRoute(ctx, adminClient, cmd, plan, staticServer.Port(), os.Getpid(), stdout, stderr)
		if err != nil {
			return err
		}
		defer cleanup()
		<-ctx.Done()
		return nil
	}

	childStdout := newLimitedCapture(32 * 1024)
	childStderr := newLimitedCapture(32 * 1024)

	result, err := startRunnerFunc(ctx, runner.Config{
		Command:             plan.Command,
		Env:                 plan.Env,
		ChosenPort:          plan.Port,
		RequireDetectedPort: plan.RequireDetectedPort,
		Stdout:              childStdout,
		Stderr:              childStderr,
		StartupTimeout:      15 * time.Second,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		replayCapturedOutput(stderr, childStdout, childStderr)
		return formatRunError(err)
	}
	defer result.Stop()

	cleanup, err := registerRoute(ctx, adminClient, cmd, plan, result.Port, result.PID(), stdout, stderr)
	if err != nil {
		return err
	}
	defer cleanup()
	return result.Wait()
}

func registerRoute(ctx context.Context, adminClient adminClient, cmd cli.Command, plan RunPlan, port, pid int, stdout, stderr io.Writer) (func(), error) {
	routes, err := adminClient.Routes(ctx)
	if err != nil {
		if errors.Is(err, admin.ErrUnauthorized) {
			return nil, staleRouterTokenError()
		}
		return nil, err
	}
	plan.Host = resolveRouteHost(plan, toRegisteredRoutes(routes))
	route := router.Route{
		Host:      plan.Host,
		Target:    routeTarget(plan.RouteTargetHost, port),
		PID:       pid,
		CWD:       plan.CWD,
		Name:      plan.Name,
		Source:    plan.RouteSource,
		OwnerCWD:  plan.CWD,
		OwnerEnv:  plan.OwnerEnv,
		StartedAt: time.Now().UTC(),
	}
	if err := adminClient.UpsertRoute(ctx, route); err != nil {
		if errors.Is(err, admin.ErrUnauthorized) {
			return nil, staleRouterTokenError()
		}
		return nil, err
	}

	publicURL := publicRouteURL(route.Host, plan.URLPath)
	fmt.Fprint(stdout, runSuccessOutput(cmd, route.Host, plan.URLPath))
	if cmd.Open {
		if err := openBrowserFunc(ctx, publicURL); err != nil {
			fmt.Fprintf(stderr, "Could not open browser automatically.\nOpen manually: %s\n", publicURL)
		}
	}
	if cmd.Verbose {
		fmt.Fprintf(stdout, "\ntarget: %s\n", route.Target)
		if plan.ProjectRoot != "" {
			fmt.Fprintf(stdout, "project root: %s\n", plan.ProjectRoot)
		}
		fmt.Fprintf(stdout, "command: %s\n", strings.Join(plan.Command, " "))
		routerLabel := plan.RouterLabel
		if routerLabel == "" {
			routerLabel = "running"
		}
		fmt.Fprintf(stdout, "router: %s\n", routerLabel)
	}
	return func() {
		adminClient.DeleteRoute(context.Background(), route.Host)
	}, nil
}

func resolveRunRouter(ctx context.Context, stderr io.Writer) (runRouter, error) {
	local := func() (runRouter, error) {
		client, err := defaultAdminClientFunc()
		health := routerHealthFunc
		if err == nil {
			health = client.Health
		} else {
			client = nil
		}
		if err := ensureRouter(ctx, stderr, health); err != nil {
			return runRouter{}, err
		}
		if client == nil {
			client, err = defaultAdminClientFunc()
			if err != nil {
				return runRouter{}, staleRouterTokenError()
			}
		}
		return runRouter{
			Client:          client,
			RouteTargetHost: "127.0.0.1",
			ChildHost:       "127.0.0.1",
			RouterLabel:     "running",
		}, nil
	}

	if !detectWSLFunc() {
		return local()
	}

	token, _, err := discoverWindowsTokenFunc(windowsUsersRoot)
	if err != nil {
		if errors.Is(err, bridge.ErrWindowsTokenNotFound) {
			if !windowsStableBinaryExists(windowsUsersRoot) {
				return local()
			}
			if healthErr := windowsRouterHealthFunc(ctx); healthErr != nil {
				return local()
			}
		}
		return runRouter{}, windowsTokenError(err)
	}
	if !windowsStableBinaryExists(windowsUsersRoot) {
		return local()
	}
	if err := windowsRouterHealthFunc(ctx); err != nil {
		if !windowsStableBinaryExists(windowsUsersRoot) {
			return local()
		}
		return runRouter{}, windowsRouterUnavailableError()
	}
	client := newWindowsAdminClientFunc(token)
	if _, err := client.Routes(ctx); err != nil {
		if errors.Is(err, admin.ErrUnauthorized) {
			return runRouter{}, windowsTokenError(err)
		}
		return runRouter{}, err
	}
	wslIP, err := currentWSLIPFunc(ctx)
	if err != nil {
		return runRouter{}, err
	}
	targetHost, err := bridgeTargetHost(ctx, client, wslIP)
	if err != nil {
		return runRouter{}, err
	}
	return runRouter{
		Client:          client,
		RouteTargetHost: targetHost,
		ChildHost:       "0.0.0.0",
		RouteSource:     "wsl",
		OwnerEnv:        "wsl",
		RouterLabel:     "Windows",
		Bridge:          true,
	}, nil
}

func bridgeTargetHost(ctx context.Context, client bridgeProbeClient, wslIP string) (string, error) {
	for _, candidate := range bridgeTargetCandidates(wslIP) {
		reachable, _, err := probeBridgeFunc(ctx, client, candidate)
		if err != nil {
			return "", err
		}
		if reachable {
			return candidate, nil
		}
	}
	return "", windowsRouterCannotReachWSLError()
}

func bridgeTargetCandidates(wslIP string) []string {
	candidates := []string{"127.0.0.1", "localhost"}
	if wslIP != "" && wslIP != "127.0.0.1" && wslIP != "localhost" {
		candidates = append(candidates, wslIP)
	}
	return candidates
}

func applyRunRouter(plan *RunPlan, rr runRouter) {
	if rr.RouteTargetHost != "" {
		plan.RouteTargetHost = rr.RouteTargetHost
	}
	if rr.RouteSource != "" {
		plan.RouteSource = rr.RouteSource
	}
	if rr.OwnerEnv != "" {
		plan.OwnerEnv = rr.OwnerEnv
	}
	if rr.RouterLabel != "" {
		plan.RouterLabel = rr.RouterLabel
	}
	if rr.ChildHost != "" && plan.Static {
		plan.StaticBindHost = rr.ChildHost
	}
	if rr.ChildHost != "" && !plan.Static {
		plan.Env = runner.ChildEnvForHost(os.Environ(), plan.Port, rr.ChildHost)
		plan.Command = withHost(plan.Command, rr.ChildHost)
	}
}

func withHost(command []string, host string) []string {
	if host == "" || host == "127.0.0.1" {
		return command
	}
	out := append([]string(nil), command...)
	for i := range out {
		if out[i] == "127.0.0.1" {
			out[i] = host
		}
	}
	return out
}

func routeTarget(host string, port int) string {
	if host == "" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, fmt.Sprint(port))
}

func windowsTokenError(err error) error {
	return errors.New("Windows gohere router found, but WSL could not read or use its token.\nRun gohere from Windows or use the WSL router.\n\nIf you use gohere in both Windows and WSL, run gohere uninstall in the side where the old router is running.")
}

func windowsRouterUnavailableError() error {
	return errors.New("Windows gohere is installed, but its router is not running.\nRun gohere from Windows first, or run gohere uninstall from Windows to use the WSL router.")
}

func windowsRouterCannotReachWSLError() error {
	return errors.New("Windows gohere router is running, but cannot reach WSL dev servers.\nTry enabling mirrored networking in %USERPROFILE%\\.wslconfig:\n  [wsl2]\n  networkingMode=mirrored\nThen run:\n  wsl --shutdown")
}

func staleRouterTokenError() error {
	return errors.New("gohere found a router it cannot control.\n\nThis usually means another gohere install is already running, or Windows and WSL are each using a different gohere token.\n\nStop the old router, then run gohere again.\nTry:\n  gohere uninstall\n\nIf you use gohere in both Windows and WSL, run that command in the side where the old router is running. If that does not work, stop the process using ports 80 and 39399.")
}

type registeredRoute struct {
	Host string
	CWD  string
}

func resolveRouteHost(plan RunPlan, routes []registeredRoute) string {
	active := make(map[string]string, len(routes))
	for _, route := range routes {
		active[route.Host] = route.CWD
	}
	return project.ResolveHostnameConflict(plan.Host, plan.CWD, active)
}

func toRegisteredRoutes(routes []router.Route) []registeredRoute {
	registered := make([]registeredRoute, 0, len(routes))
	for _, route := range routes {
		registered = append(registered, registeredRoute{Host: route.Host, CWD: route.CWD})
	}
	return registered
}

func runSuccessOutput(cmd cli.Command, host, urlPath string) string {
	label := "gohere"
	if cmd.Kind == cli.CommandRun && cmd.Script != "" && (cmd.Script != "dev" || urlPath != "") {
		label += " " + cmd.Script
	}
	return fmt.Sprintf("%s \u2192 http://%s%s\n", label, host, escapedURLPath(urlPath))
}

func publicRouteURL(host, urlPath string) string {
	return fmt.Sprintf("http://%s%s", host, escapedURLPath(urlPath))
}

func isFileTarget(cmd cli.Command) bool {
	return cmd.Kind == cli.CommandRun && cmd.Script != "" && filepath.Ext(cmd.Script) != ""
}

func prepareStaticFileTarget(cmd cli.Command, cwd string, port int) (RunPlan, error) {
	cleanPath := filepath.Clean(cmd.Script)
	if filepath.IsAbs(cleanPath) || cleanPath == ".." || strings.HasPrefix(cleanPath, ".."+string(filepath.Separator)) {
		return RunPlan{}, fmt.Errorf("File not found: %s", cmd.Script)
	}

	fullPath := filepath.Join(cwd, cleanPath)
	info, err := os.Stat(fullPath)
	if err != nil || info.IsDir() {
		return RunPlan{}, fmt.Errorf("File not found: %s", cmd.Script)
	}

	host := project.NormalizeHostnameName(filepath.Base(cwd)) + ".localhost"
	return RunPlan{
		Port:    port,
		Host:    host,
		Name:    strings.TrimSuffix(host, ".localhost"),
		CWD:     cwd,
		Static:  true,
		URLPath: "/" + filepath.ToSlash(cleanPath),
	}, nil
}

func escapedURLPath(path string) string {
	if path == "" {
		return ""
	}
	return (&url.URL{Path: path}).EscapedPath()
}

func missingScriptError(script string, available []string) error {
	if len(available) > 3 {
		var out strings.Builder
		fmt.Fprintf(&out, "gohere error: script %q not found.\n\nAvailable scripts:", script)
		for _, item := range available {
			fmt.Fprintf(&out, "\n  %s", item)
		}
		return errors.New(out.String())
	}
	return fmt.Errorf("gohere error: script %q not found; available scripts: %s", script, strings.Join(available, ", "))
}

func formatRunError(err error) error {
	msg := err.Error()
	if strings.Contains(msg, "could not detect a local URL") {
		return errors.New("gohere error: started dev script, but could not detect a local URL.\nTry:\n  gohere --target 5173")
	}
	return fmt.Errorf("gohere error: %w", err)
}

func replayCapturedOutput(out io.Writer, captures ...*limitedCapture) {
	wrote := false
	for _, capture := range captures {
		text := capture.String()
		if text == "" {
			continue
		}
		if wrote && !strings.HasSuffix(text, "\n") {
			fmt.Fprintln(out)
		}
		fmt.Fprint(out, text)
		if !strings.HasSuffix(text, "\n") {
			fmt.Fprintln(out)
		}
		wrote = true
	}
	if wrote {
		fmt.Fprintln(out)
	}
}

type limitedCapture struct {
	buf bytes.Buffer
	max int
}

func newLimitedCapture(max int) *limitedCapture {
	return &limitedCapture{max: max}
}

func (w *limitedCapture) Write(p []byte) (int, error) {
	accepted := len(p)
	remaining := w.max - w.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		w.buf.Write(p)
	}
	return accepted, nil
}

func (w *limitedCapture) String() string {
	return w.buf.String()
}

func ensureRouter(ctx context.Context, out io.Writer, health func(context.Context) error) error {
	if err := health(ctx); err == nil {
		return nil
	}
	if err := startInstalledRouterFunc(ctx); err == nil {
		if err := waitForRouterHealth(ctx, health, 3*time.Second); err == nil {
			return nil
		} else {
			return installedRouterUnavailableError(err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return installedRouterUnavailableError(err)
	}

	fmt.Fprint(out, firstRunPrompt())
	answer, _ := bufio.NewReader(promptInput).ReadString('\n')
	if !shouldRunSetupFromAnswer(answer) {
		fmt.Fprint(out, "gohere was not enabled.\n\nRun gohere again when you are ready.\n")
		return errors.New("gohere was not enabled")
	}
	if err := setupFunc(ctx); err != nil {
		return err
	}
	if err := waitForRouterHealth(ctx, health, 3*time.Second); err != nil {
		return errors.New("gohere setup finished, but the router is still not reachable")
	}
	fmt.Fprintln(out)
	return nil
}

func installedRouterUnavailableError(err error) error {
	return fmt.Errorf("installed gohere router is not reachable.\nTry:\n  gohere doctor\n\nDetails: %w", err)
}

func startInstalledRouter(ctx context.Context) error {
	return setup.StartInstalledRouter(ctx, setup.Config{}, stableBinaryName(runtime.GOOS))
}

func waitForRouterHealth(ctx context.Context, health func(context.Context) error, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if err := health(ctx); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return lastErr
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func firstRunPrompt() string {
	return "gohere needs one-time permission to enable .localhost project URLs.\nThis lets gohere use port 80 locally. Continue? [Y/n] "
}

func shouldRunSetupFromAnswer(answer string) bool {
	answer = strings.TrimSpace(strings.ToLower(answer))
	return answer == "" || answer == "y" || answer == "yes"
}

func setupForGOOS(ctx context.Context, goos string) error {
	cfg := setup.Config{
		Stderr: os.Stderr,
		RouterHealth: func(ctx context.Context) error {
			client, err := defaultAdminClient()
			if err != nil {
				return err
			}
			return client.Health(ctx)
		},
	}
	switch goos {
	case "linux":
		cfg.SystemdAvailable = systemdUserAvailable()
		return setupLinuxFunc(ctx, cfg)
	case "windows":
		return setupWindowsFunc(ctx, cfg)
	default:
		return fmt.Errorf("gohere setup is not supported on %s yet", goos)
	}
}

func List(stdout io.Writer, verbose bool) error {
	manager, err := resolveRouteManager(context.Background())
	if err != nil {
		return err
	}
	if manager.Client != nil {
		routes, err := manager.Client.Routes(context.Background())
		if err != nil {
			if errors.Is(err, admin.ErrUnauthorized) {
				return staleRouterTokenError()
			}
			return err
		}
		printRoutes(stdout, routes, verbose)
		return nil
	}
	return ListWithStoreRouterReady(stdout, manager.Store, verbose, manager.RouterReady)
}

func ListWithStore(stdout io.Writer, store router.Store, verbose bool) error {
	return ListWithStoreRouterReady(stdout, store, verbose, true)
}

func ListWithStoreRouterReady(stdout io.Writer, store router.Store, verbose bool, routerReady bool) error {
	routes, err := store.Load()
	if err != nil {
		return err
	}
	printRoutesWithRouterReady(stdout, routes, verbose, routerReady)
	return nil
}

func printRoutes(stdout io.Writer, routes []router.Route, verbose bool) {
	printRoutesWithRouterReady(stdout, routes, verbose, true)
}

func printRoutesWithRouterReady(stdout io.Writer, routes []router.Route, verbose bool, routerReady bool) {
	statuses := lifecycle.RouteStatusesWithRouterReady(routes, routerReady)
	if verbose {
		fmt.Fprint(stdout, lifecycle.FormatRoutesVerbose(statuses))
		return
	}
	fmt.Fprint(stdout, lifecycle.FormatRoutes(statuses))
}

func Prune(stdout io.Writer) error {
	manager, err := resolveRouteManager(context.Background())
	if err != nil {
		return err
	}
	if manager.Client != nil {
		removed, err := pruneAdminRoutes(context.Background(), manager.Client)
		if err != nil {
			return err
		}
		return printPruneResult(stdout, removed)
	}
	removed, err := lifecycle.PruneWithRouterReady(manager.Store, manager.RouterReady)
	if err != nil {
		return err
	}
	return printPruneResult(stdout, removed)
}

func printPruneResult(stdout io.Writer, removed int) error {
	switch removed {
	case 0:
		fmt.Fprintln(stdout, "No dead routes.")
	case 1:
		fmt.Fprintln(stdout, "Removed 1 dead route.")
	default:
		fmt.Fprintf(stdout, "Removed %d dead routes.\n", removed)
	}
	return nil
}

func Stop(cwd string, stdout io.Writer) error {
	manager, err := resolveRouteManager(context.Background())
	if err != nil {
		return err
	}
	var host string
	var stopped bool
	var warning string
	if manager.Client != nil {
		host, stopped, warning, err = stopAdminCurrent(context.Background(), manager.Client, cwd)
	} else {
		host, stopped, warning, err = lifecycle.StopCurrent(manager.Store, cwd)
	}
	if err != nil {
		if host != "" {
			return fmt.Errorf("gohere error: could not stop %s.\nTry:\n  gohere doctor", host)
		}
		return fmt.Errorf("gohere error: %w", err)
	}
	printStopResult(stdout, host, stopped, warning)
	return nil
}

type routeManager struct {
	Client      adminClient
	Store       router.Store
	RouterReady bool
}

func resolveRouteManager(ctx context.Context) (routeManager, error) {
	local := func() routeManager {
		manager := routeManager{Store: defaultStore()}
		client, err := defaultAdminClientFunc()
		if err == nil && client.Health(ctx) == nil {
			manager.Client = client
			manager.RouterReady = true
		}
		return manager
	}
	if !detectWSLFunc() {
		return local(), nil
	}
	token, _, err := discoverWindowsTokenFunc(windowsUsersRoot)
	if err != nil {
		if errors.Is(err, bridge.ErrWindowsTokenNotFound) {
			if !windowsStableBinaryExists(windowsUsersRoot) {
				return local(), nil
			}
			if healthErr := windowsRouterHealthFunc(ctx); healthErr != nil {
				return local(), nil
			}
		}
		return routeManager{}, windowsTokenError(err)
	}
	if !windowsStableBinaryExists(windowsUsersRoot) {
		return local(), nil
	}
	if err := windowsRouterHealthFunc(ctx); err != nil {
		if !windowsStableBinaryExists(windowsUsersRoot) {
			return local(), nil
		}
		return routeManager{}, windowsRouterUnavailableError()
	}
	client := newWindowsAdminClientFunc(token)
	if _, err := client.Routes(ctx); err != nil {
		if errors.Is(err, admin.ErrUnauthorized) {
			return routeManager{}, windowsTokenError(err)
		}
		return routeManager{}, err
	}
	return routeManager{Client: client, RouterReady: true}, nil
}

func pruneAdminRoutes(ctx context.Context, client adminClient) (int, error) {
	routes, err := client.Routes(ctx)
	if err != nil {
		if errors.Is(err, admin.ErrUnauthorized) {
			return 0, staleRouterTokenError()
		}
		return 0, err
	}
	removed := 0
	for _, status := range lifecycle.RouteStatuses(routes) {
		if status.Status != lifecycle.RouteStatusDead {
			continue
		}
		if err := client.DeleteRoute(ctx, status.Route.Host); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

func stopAdminCurrent(ctx context.Context, client adminClient, cwd string) (string, bool, string, error) {
	routes, err := client.Routes(ctx)
	if err != nil {
		if errors.Is(err, admin.ErrUnauthorized) {
			return "", false, "", staleRouterTokenError()
		}
		return "", false, "", err
	}
	absCWD, err := filepath.Abs(cwd)
	if err != nil {
		return "", false, "", err
	}
	for _, route := range routes {
		routeCWD := route.OwnerCWD
		if routeCWD == "" {
			routeCWD = route.CWD
		}
		absRouteCWD, err := filepath.Abs(routeCWD)
		if err != nil || absRouteCWD != absCWD {
			continue
		}
		if route.OwnerEnv != "" && route.OwnerEnv != "wsl" {
			continue
		}
		if !lifecycle.PIDAlive(route.PID) || lifecycle.RouteStatuses([]router.Route{route})[0].Status == lifecycle.RouteStatusDead {
			if err := client.DeleteRoute(ctx, route.Host); err != nil {
				return route.Host, false, "", err
			}
			return route.Host, false, "", nil
		}
		if lifecycle.RouteProcessVerified(route) {
			lifecycle.StopPID(route.PID)
			if err := client.DeleteRoute(ctx, route.Host); err != nil {
				return route.Host, false, "", err
			}
			return route.Host, true, "", nil
		}
		return route.Host, false, lifecycle.UnverifiedProcessWarning(route.PID), nil
	}
	return "", false, "", nil
}

func printStopResult(stdout io.Writer, host string, stopped bool, warning string) {
	if warning != "" {
		fmt.Fprintln(stdout, warning)
		return
	}
	if host == "" {
		fmt.Fprintln(stdout, "No running gohere app found for this folder.")
		return
	}
	fmt.Fprintf(stdout, "Stopped %s.\n", host)
}

func Doctor(stdout io.Writer) error {
	stateDir := router.DefaultStateDir()
	client, err := defaultAdminClientFunc()
	if err != nil {
		client = nil
	}
	return DoctorWithStore(stdout, stateDir, defaultStore(), client)
}

func DoctorWithStore(stdout io.Writer, stateDir string, store router.Store, client adminClient) error {
	return DoctorWithChecks(stdout, stateDir, store, client, DoctorChecks{Port80Status: port80Status})
}

type Port80Status struct {
	OK     bool
	Detail string
	Hint   string
}

type DoctorChecks struct {
	Port80Available      func() bool
	Port80Status         func() Port80Status
	SetcapEnabled        func(string) bool
	SystemdUserServiceOK func() (bool, bool)
	GOOS                 string
}

func DoctorWithChecks(stdout io.Writer, stateDir string, store router.Store, client adminClient, extra DoctorChecks) error {
	goos := extra.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	if extra.Port80Available == nil {
		extra.Port80Available = port80Available
	}
	if extra.SetcapEnabled == nil {
		extra.SetcapEnabled = setcapEnabled
	}
	if extra.SystemdUserServiceOK == nil {
		extra.SystemdUserServiceOK = systemdUserServiceOK
	}
	tokenPath := filepath.Join(stateDir, "token")
	binaryPath := filepath.Join(stateDir, "bin", stableBinaryName(goos))
	pidPath := filepath.Join(stateDir, "router.pid")
	checks := []lifecycle.DoctorCheck{
		{Name: "state dir", OK: exists(stateDir), Detail: stateDir, Hint: "Try: run gohere once to finish setup."},
		{Name: "stable binary", OK: exists(binaryPath), Detail: binaryPath, Hint: "Try: run gohere once to reinstall the local router binary."},
		{Name: "token", OK: exists(tokenPath), Detail: tokenPath, Hint: "Try: run gohere uninstall, then run gohere again."},
	}
	if info, err := os.Stat(tokenPath); goos != "windows" && err == nil {
		checks = append(checks, lifecycle.DoctorCheck{Name: "token permissions", OK: info.Mode().Perm() == 0600, Detail: info.Mode().Perm().String(), Hint: "Try: chmod 600 ~/.gohere/token"})
	}
	adminHealthy := false
	if client != nil {
		adminHealthy = client.Health(context.Background()) == nil
		checks = append(checks, lifecycle.DoctorCheck{Name: "admin API health", OK: adminHealthy, Hint: "Try: gohere uninstall, then run gohere again."})
	}
	if pid, err := os.ReadFile(pidPath); err == nil {
		checks = append(checks, lifecycle.DoctorCheck{Name: "router pid", OK: true, Detail: strings.TrimSpace(string(pid))})
	} else {
		checks = append(checks, lifecycle.DoctorCheck{Name: "router pid", OK: false, Detail: pidPath, Hint: "Try: run gohere once to start the router."})
	}
	if routes, err := store.Load(); err == nil {
		checks = append(checks, lifecycle.DoctorCheck{Name: "active routes", OK: true, Detail: fmt.Sprintf("%d", len(routes))})
	}
	if extra.Port80Status != nil {
		status := extra.Port80Status()
		ok := status.OK
		detail := status.Detail
		hint := status.Hint
		if !ok && adminHealthy {
			ok = true
			detail = "used by gohere router"
			hint = ""
		}
		checks = append(checks, lifecycle.DoctorCheck{Name: "port 80", OK: ok, Detail: detail, Hint: hint})
	} else if extra.Port80Available != nil {
		detail := "blocked"
		ok := extra.Port80Available()
		if ok {
			detail = "available"
		} else if adminHealthy {
			ok = true
			detail = "used by gohere router"
		}
		checks = append(checks, lifecycle.DoctorCheck{Name: "port 80", OK: ok, Detail: detail, Hint: "Try: stop the process using port 80, then run gohere again."})
	}
	if goos == "linux" && exists(binaryPath) {
		checks = append(checks, lifecycle.DoctorCheck{Name: "setcap", OK: extra.SetcapEnabled(binaryPath), Detail: "cap_net_bind_service", Hint: "Try: sudo setcap cap_net_bind_service=+ep ~/.gohere/bin/gohere"})
	}
	if goos == "linux" {
		applicable, ok := extra.SystemdUserServiceOK()
		if applicable {
			detail := "inactive"
			if ok {
				detail = "active"
			}
			checks = append(checks, lifecycle.DoctorCheck{Name: "systemd user service", OK: ok, Detail: detail, Hint: "Try: systemctl --user restart gohere-router"})
		}
	}
	checks = append(checks, bridgeDoctorChecks(context.Background())...)
	fmt.Fprint(stdout, lifecycle.FormatDoctor(checks))
	return nil
}

func bridgeDoctorChecks(ctx context.Context) []lifecycle.DoctorCheck {
	if !detectWSLFunc() {
		return nil
	}
	checks := []lifecycle.DoctorCheck{{Name: "environment", OK: true, Detail: "WSL"}}
	if !windowsStableBinaryExists(windowsUsersRoot) {
		return checks
	}
	if err := windowsRouterHealthFunc(ctx); err != nil {
		return checks
	}
	token, _, err := discoverWindowsTokenFunc(windowsUsersRoot)
	if err != nil {
		if errors.Is(err, bridge.ErrWindowsTokenNotFound) {
			return checks
		}
		return append(checks, lifecycle.DoctorCheck{Name: "windows router", OK: false, Detail: "token unavailable", Hint: "Try: run gohere from Windows or stop the Windows router."})
	}
	client := newWindowsAdminClientFunc(token)
	if _, err := client.Routes(ctx); err != nil {
		return append(checks, lifecycle.DoctorCheck{Name: "windows router", OK: false, Detail: "auth failed", Hint: "Try: gohere uninstall in the side where the old router is running."})
	}
	return append(checks, lifecycle.DoctorCheck{Name: "windows router", OK: true, Detail: "available"})
}

func stableBinaryName(goos string) string {
	if goos == "windows" {
		return "gohere.exe"
	}
	return "gohere"
}

func Setup(ctx context.Context) error {
	return setupFunc(ctx)
}

func projectDir(packagePath string) string {
	return filepath.Dir(packagePath)
}

func defaultStore() router.Store {
	return router.NewRouteStore(filepath.Join(router.DefaultStateDir(), "routes.json"))
}

func defaultAdminClient() (*admin.Client, error) {
	token, err := router.ReadToken(router.DefaultStateDir())
	if err != nil {
		return nil, err
	}
	return admin.NewClient("http://127.0.0.1:39399", token), nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func systemdUserAvailable() bool {
	return systemdUserAvailableAt("/run/user", os.Getuid())
}

func systemdUserAvailableAt(runUserRoot string, uid int) bool {
	_, err := os.Stat(filepath.Join(runUserRoot, fmt.Sprintf("%d", uid), "bus"))
	return err == nil
}

func port80Available() bool {
	ln, err := net.Listen("tcp", "127.0.0.1:80")
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

func port80Status() Port80Status {
	ln, err := net.Listen("tcp", "127.0.0.1:80")
	if err == nil {
		ln.Close()
		return Port80Status{OK: true, Detail: "available"}
	}
	if isPermissionBindError(err) {
		return Port80Status{OK: false, Detail: "permission required", Hint: "Try: gohere setup"}
	}
	if isAddressInUseError(err) {
		return Port80Status{OK: false, Detail: "already in use", Hint: "Try: stop the process using port 80, then run gohere again."}
	}
	return Port80Status{OK: false, Detail: "bind failed", Hint: fmt.Sprintf("Bind error: %v", err)}
}

func isPermissionBindError(err error) bool {
	if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EACCES) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "permission denied") || strings.Contains(msg, "access is denied")
}

func isAddressInUseError(err error) bool {
	if errors.Is(err, syscall.EADDRINUSE) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "address already in use") ||
		strings.Contains(msg, "only one usage of each socket address")
}

func setcapEnabled(path string) bool {
	output, err := exec.Command("getcap", path).Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(output), "cap_net_bind_service")
}

func systemdUserServiceOK() (bool, bool) {
	if !systemdUserAvailable() {
		return false, false
	}
	err := exec.Command("systemctl", "--user", "is-active", "--quiet", "gohere-router").Run()
	return true, err == nil
}
