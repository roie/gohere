package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
)

type RunPlan struct {
	Command []string
	Env     []string
	Port    int
	Host    string
	Name    string
	CWD     string
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
		return RunPlan{Command: append([]string(nil), cmd.Raw...), Env: env, Port: port, Host: host, Name: strings.TrimSuffix(host, ".localhost"), CWD: cwd}, nil
	}

	packagePath, found, err := project.FindNearestPackageJSON(cwd)
	if err != nil {
		return RunPlan{}, err
	}
	if !found {
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

	result, err := runner.Start(ctx, runner.Config{
		Command:        plan.Command,
		Env:            plan.Env,
		ChosenPort:     plan.Port,
		Stdout:         stdout,
		Stderr:         stderr,
		StartupTimeout: 15 * time.Second,
	})
	if err != nil {
		return err
	}
	defer result.Stop()

	adminClient, err := defaultAdminClient()
	if err != nil {
		return err
	}
	if err := adminClient.Health(ctx); err != nil {
		return errors.New("gohere router is not running; run gohere setup")
	}
	route := router.Route{
		Host:      plan.Host,
		Target:    fmt.Sprintf("http://127.0.0.1:%d", result.Port),
		PID:       result.PID(),
		CWD:       plan.CWD,
		Name:      plan.Name,
		StartedAt: time.Now().UTC(),
	}
	if err := adminClient.UpsertRoute(ctx, route); err != nil {
		return err
	}
	defer adminClient.DeleteRoute(ctx, route.Host)

	if cmd.Verbose {
		fmt.Fprintf(stderr, "target: http://127.0.0.1:%d\n", result.Port)
	}
	return result.Wait()
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
	tokenPath := filepath.Join(stateDir, "token")
	binaryPath := filepath.Join(stateDir, "bin", "gohere")
	checks := []lifecycle.DoctorCheck{
		{Name: "state dir", OK: exists(stateDir), Detail: stateDir},
		{Name: "stable binary", OK: exists(binaryPath), Detail: binaryPath},
		{Name: "token", OK: exists(tokenPath), Detail: tokenPath},
	}
	if info, err := os.Stat(tokenPath); err == nil {
		checks = append(checks, lifecycle.DoctorCheck{Name: "token permissions", OK: info.Mode().Perm() == 0600, Detail: info.Mode().Perm().String()})
	}
	if client, err := defaultAdminClient(); err == nil {
		checks = append(checks, lifecycle.DoctorCheck{Name: "admin API health", OK: client.Health(context.Background()) == nil})
	}
	fmt.Fprint(stdout, lifecycle.FormatDoctor(checks))
	return nil
}

func Setup(ctx context.Context) error {
	return setup.Linux(ctx, setup.Config{SystemdAvailable: systemdUserAvailable()})
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
	_, err := os.Stat("/run/user")
	return err == nil
}
