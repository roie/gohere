package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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
		return setup.Linux(ctx, setup.Config{
			SystemdAvailable: systemdUserAvailable(),
			Stderr:           os.Stderr,
			RouterHealth: func(ctx context.Context) error {
				client, err := defaultAdminClient()
				if err != nil {
					return err
				}
				return client.Health(ctx)
			},
		})
	}
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
	Static              bool
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

	packagePath, found, err := project.FindNearestPackageJSON(cwd)
	if err != nil {
		return RunPlan{}, err
	}
	if !found {
		if staticserver.IsStaticProject(cwd) {
			host := project.NormalizeHostnameName(filepath.Base(cwd)) + ".localhost"
			return RunPlan{Port: port, Host: host, Name: strings.TrimSuffix(host, ".localhost"), CWD: cwd, Static: true}, nil
		}
		return RunPlan{}, errors.New("no package.json found; use gohere -- <command>")
	}

	pkg, err := project.ReadPackageJSON(packagePath)
	if err != nil {
		return RunPlan{}, err
	}
	scriptCommand, ok := pkg.Script(cmd.Script)
	if !ok {
		return RunPlan{}, fmt.Errorf("script %s not found; available scripts: %s", cmd.Script, strings.Join(pkg.AvailableScripts(), ", "))
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
	return RunPlan{Command: command, Env: env, Port: port, Host: host, Name: strings.TrimSuffix(host, ".localhost"), CWD: cwd}, nil
}

func Run(ctx context.Context, cmd cli.Command, cwd string, stdout, stderr io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	plan, err := PrepareRun(cmd, cwd)
	if err != nil {
		return err
	}

	if cmd.Verbose {
		fmt.Fprintf(stderr, "hidden port: %d\n", plan.Port)
		fmt.Fprintf(stderr, "command: %s\n", strings.Join(plan.Command, " "))
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
		cleanup, err := registerRoute(ctx, adminClient, plan, staticServer.Port(), 0, cmd.Verbose, stdout, stderr)
		if err != nil {
			return err
		}
		defer cleanup()
		<-ctx.Done()
		return nil
	}

	result, err := startRunnerFunc(ctx, runner.Config{
		Command:             plan.Command,
		Env:                 plan.Env,
		ChosenPort:          plan.Port,
		RequireDetectedPort: plan.RequireDetectedPort,
		Stdout:              stdout,
		Stderr:              stderr,
		StartupTimeout:      15 * time.Second,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	defer result.Stop()

	cleanup, err := registerRoute(ctx, adminClient, plan, result.Port, result.PID(), cmd.Verbose, stdout, stderr)
	if err != nil {
		return err
	}
	defer cleanup()
	return result.Wait()
}

func registerRoute(ctx context.Context, adminClient adminClient, plan RunPlan, port, pid int, verbose bool, stdout, stderr io.Writer) (func(), error) {
	routes, err := adminClient.Routes(ctx)
	if err != nil {
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
		return nil, err
	}

	if verbose {
		fmt.Fprintf(stderr, "target: http://127.0.0.1:%d\n", port)
	}
	fmt.Fprint(stdout, runSuccessOutput(plan.Name, route.Host))
	return func() {
		adminClient.DeleteRoute(context.Background(), route.Host)
	}, nil
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

func runSuccessOutput(name, host string) string {
	return fmt.Sprintf("%s is running\n\nhttp://%s\n", name, host)
}

func ensureRouter(ctx context.Context, out io.Writer, health func(context.Context) error) error {
	if err := health(ctx); err == nil {
		return nil
	}

	fmt.Fprint(out, firstRunPrompt())
	answer, _ := bufio.NewReader(promptInput).ReadString('\n')
	if !shouldRunSetupFromAnswer(answer) {
		return errors.New("gohere router is not running; run gohere setup")
	}
	if err := setupFunc(ctx); err != nil {
		return err
	}
	if err := health(ctx); err != nil {
		return errors.New("gohere setup finished, but the router is still not reachable")
	}
	return nil
}

func firstRunPrompt() string {
	return `Clean local URLs are not enabled yet.
gohere can enable:
  http://myproject.localhost

This requires one-time system permission.
It will not run your project as root.

Enable now? [Y/n] `
}

func shouldRunSetupFromAnswer(answer string) bool {
	answer = strings.TrimSpace(strings.ToLower(answer))
	return answer == "" || answer == "y" || answer == "yes"
}

func List(stdout io.Writer) error {
	store := defaultStore()
	routes, err := store.Load()
	if err != nil {
		return err
	}
	fmt.Fprint(stdout, lifecycle.FormatRoutes(lifecycle.RouteStatuses(routes)))
	return nil
}

func Clean(stdout io.Writer) error {
	removed, err := lifecycle.Clean(defaultStore())
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Removed %d dead route(s).\n", removed)
	return nil
}

func Stop(cwd string, stdout io.Writer) error {
	stopped, err := lifecycle.StopCurrent(defaultStore(), cwd)
	if err != nil {
		return err
	}
	if !stopped {
		fmt.Fprintln(stdout, "No running gohere-managed process found for this folder.")
	}
	return nil
}

func Doctor(stdout io.Writer) error {
	stateDir := router.DefaultStateDir()
	client, _ := defaultAdminClientFunc()
	return DoctorWithStore(stdout, stateDir, defaultStore(), client)
}

func DoctorWithStore(stdout io.Writer, stateDir string, store router.Store, client adminClient) error {
	return DoctorWithChecks(stdout, stateDir, store, client, DoctorChecks{Port80Available: port80Available})
}

type DoctorChecks struct {
	Port80Available      func() bool
	SetcapEnabled        func(string) bool
	SystemdUserServiceOK func() (bool, bool)
}

func DoctorWithChecks(stdout io.Writer, stateDir string, store router.Store, client adminClient, extra DoctorChecks) error {
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
	binaryPath := filepath.Join(stateDir, "bin", "gohere")
	pidPath := filepath.Join(stateDir, "router.pid")
	checks := []lifecycle.DoctorCheck{
		{Name: "state dir", OK: exists(stateDir), Detail: stateDir},
		{Name: "stable binary", OK: exists(binaryPath), Detail: binaryPath},
		{Name: "token", OK: exists(tokenPath), Detail: tokenPath},
	}
	if info, err := os.Stat(tokenPath); err == nil {
		checks = append(checks, lifecycle.DoctorCheck{Name: "token permissions", OK: info.Mode().Perm() == 0600, Detail: info.Mode().Perm().String()})
	}
	adminHealthy := false
	if client != nil {
		adminHealthy = client.Health(context.Background()) == nil
		checks = append(checks, lifecycle.DoctorCheck{Name: "admin API health", OK: adminHealthy})
	}
	if pid, err := os.ReadFile(pidPath); err == nil {
		checks = append(checks, lifecycle.DoctorCheck{Name: "router pid", OK: true, Detail: strings.TrimSpace(string(pid))})
	} else {
		checks = append(checks, lifecycle.DoctorCheck{Name: "router pid", OK: false, Detail: pidPath})
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
		checks = append(checks, lifecycle.DoctorCheck{Name: "port 80", OK: ok, Detail: detail})
	}
	if exists(binaryPath) {
		checks = append(checks, lifecycle.DoctorCheck{Name: "setcap", OK: extra.SetcapEnabled(binaryPath), Detail: "cap_net_bind_service"})
	}
	if applicable, ok := extra.SystemdUserServiceOK(); applicable {
		detail := "inactive"
		if ok {
			detail = "active"
		}
		checks = append(checks, lifecycle.DoctorCheck{Name: "systemd user service", OK: ok, Detail: detail})
	}
	fmt.Fprint(stdout, lifecycle.FormatDoctor(checks))
	return nil
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
