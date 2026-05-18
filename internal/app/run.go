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
	"time"

	"github.com/roie/gohere/internal/admin"
	"github.com/roie/gohere/internal/cli"
	"github.com/roie/gohere/internal/lifecycle"
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
	setupLinuxFunc                                     = setup.Linux
	setupWindowsFunc                                   = setup.Windows
	defaultAdminClientFunc func() (adminClient, error) = func() (adminClient, error) {
		return defaultAdminClient()
	}
	startRunnerFunc = runner.Start
)

type adminClient interface {
	Health(context.Context) error
	Routes(context.Context) ([]router.Route, error)
	UpsertRoute(context.Context, router.Route) error
	DeleteRoute(context.Context, string) error
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

	adminClient, err := defaultAdminClientFunc()
	if err != nil {
		return err
	}
	if err := ensureRouter(ctx, stderr, adminClient.Health); err != nil {
		return err
	}

	if plan.Static {
		staticServer, err := staticserver.Start(ctx, plan.CWD, plan.Port)
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
		Target:    fmt.Sprintf("http://127.0.0.1:%d", port),
		PID:       pid,
		CWD:       plan.CWD,
		Name:      plan.Name,
		StartedAt: time.Now().UTC(),
	}
	if err := adminClient.UpsertRoute(ctx, route); err != nil {
		if errors.Is(err, admin.ErrUnauthorized) {
			return nil, staleRouterTokenError()
		}
		return nil, err
	}

	fmt.Fprint(stdout, runSuccessOutput(cmd, route.Host, plan.URLPath))
	if cmd.Verbose {
		fmt.Fprintf(stdout, "\ntarget: http://127.0.0.1:%d\n", port)
		if plan.ProjectRoot != "" {
			fmt.Fprintf(stdout, "project root: %s\n", plan.ProjectRoot)
		}
		fmt.Fprintf(stdout, "command: %s\n", strings.Join(plan.Command, " "))
		fmt.Fprintln(stdout, "router: running")
	}
	return func() {
		adminClient.DeleteRoute(context.Background(), route.Host)
	}, nil
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
	return ListWithStore(stdout, defaultStore(), verbose)
}

func ListWithStore(stdout io.Writer, store router.Store, verbose bool) error {
	routes, err := store.Load()
	if err != nil {
		return err
	}
	statuses := lifecycle.RouteStatuses(routes)
	if verbose {
		fmt.Fprint(stdout, lifecycle.FormatRoutesVerbose(statuses))
		return nil
	}
	fmt.Fprint(stdout, lifecycle.FormatRoutes(statuses))
	return nil
}

func Clean(stdout io.Writer) error {
	removed, err := lifecycle.Clean(defaultStore())
	if err != nil {
		return err
	}
	return printCleanResult(stdout, removed)
}

func printCleanResult(stdout io.Writer, removed int) error {
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
	host, stopped, err := lifecycle.StopCurrent(defaultStore(), cwd)
	if err != nil {
		if host != "" {
			return fmt.Errorf("gohere error: could not stop %s.\nTry:\n  gohere doctor", host)
		}
		return fmt.Errorf("gohere error: %w", err)
	}
	printStopResult(stdout, host, stopped)
	return nil
}

func printStopResult(stdout io.Writer, host string, stopped bool) {
	if !stopped {
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
	return DoctorWithChecks(stdout, stateDir, store, client, DoctorChecks{Port80Available: port80Available})
}

type DoctorChecks struct {
	Port80Available      func() bool
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
	if extra.Port80Available != nil {
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
	fmt.Fprint(stdout, lifecycle.FormatDoctor(checks))
	return nil
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
	token, err := router.EnsureToken(router.DefaultStateDir())
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
