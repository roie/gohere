package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/roie/gohere/internal/admin"
	"github.com/roie/gohere/internal/bridge"
	"github.com/roie/gohere/internal/cli"
	"github.com/roie/gohere/internal/lifecycle"
	"github.com/roie/gohere/internal/opener"
	"github.com/roie/gohere/internal/probe"
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
	setupDarwinFunc                                      = setup.Darwin
	startInstalledRouterFunc                             = startInstalledRouter
	defaultAdminClientFunc   func() (adminClient, error) = func() (adminClient, error) {
		return defaultAdminClient()
	}
	startRunnerFunc            = runner.Start
	detectWSLFunc              = bridge.DetectWSL
	routerHealthFunc           = func(ctx context.Context) error { return admin.NewClient("http://127.0.0.1:39399", "").Health(ctx) }
	windowsRouterHealthFunc    = func(ctx context.Context) error { return admin.NewClient(windowsAdminBaseURL, "").Health(ctx) }
	discoverWindowsTokenFunc   = bridge.DiscoverWindowsToken
	windowsStableBinaryExists  = bridge.WindowsStableBinaryExists
	startWindowsServiceFunc    = startWindowsService
	execCommandContext         = exec.CommandContext
	windowsServiceStartTimeout = routerStartTimeout
	newWindowsAdminClientFunc  = func(token string) bridgeAdminClient { return admin.NewClient(windowsAdminBaseURL, token) }
	currentWSLIPFunc           = bridge.CurrentWSLIP
	probeBridgeFunc            = func(ctx context.Context, client bridgeProbeClient, wslIP string) (bool, string, error) {
		return bridge.ProbeBridge(ctx, client, wslIP)
	}
	localhostHTTPStatusFunc = localhostHTTPStatus
	openBrowserFunc         = func(ctx context.Context, url string) error {
		return opener.Open(ctx, runtime.GOOS, detectWSLFunc(), url)
	}
	chooseFreePortForHostFunc = runner.ChooseFreePortForHost
)

const routerStartTimeout = 10 * time.Second

const (
	doctorLocalhostProbeURL     = "http://gohere-doctor.localhost/"
	doctorLocalhostProbeTimeout = 1500 * time.Millisecond
)

type adminClient interface {
	Health(context.Context) error
	Routes(context.Context) ([]router.Route, error)
	UpsertRoute(context.Context, router.Route) error
	DeleteRoute(context.Context, string) error
}

type routeStatusClient interface {
	RouteStatuses(context.Context) ([]router.RouteStatus, error)
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
	ProjectName         string
	Static              bool
	Live                bool
	URLPath             string
	RequireDetectedPort bool
	RouteTargetHost     string
	RouteSource         string
	OwnerEnv            string
	RouterLabel         string
	StaticBindHost      string
	ManagedPort         bool
	Mode                string
}

func PrepareRun(cmd cli.Command, cwd string) (RunPlan, error) {
	return prepareRunWithHost(cmd, cwd, "127.0.0.1")
}

func prepareRunWithHost(cmd cli.Command, cwd, childHost string) (RunPlan, error) {
	port := cmd.TargetPort
	if port == 0 {
		var err error
		port, err = chooseFreePortForHostFunc(childHost)
		if err != nil {
			return RunPlan{}, err
		}
	}

	env := runner.ChildEnvForHost(os.Environ(), port, childHost)
	if cmd.Kind == cli.CommandRaw {
		host := project.NormalizeHostnameName(filepath.Base(cwd)) + ".localhost"
		return applyRunOptions(cmd, RunPlan{
			Command:             append([]string(nil), cmd.Raw...),
			Env:                 env,
			Port:                port,
			Host:                host,
			Name:                strings.TrimSuffix(host, ".localhost"),
			CWD:                 cwd,
			ProjectRoot:         cwd,
			ProjectName:         project.NormalizeHostnameName(filepath.Base(cwd)),
			RequireDetectedPort: cmd.TargetPort == 0,
		})
	}

	if cmd.TargetPath != "" {
		return preparePathTarget(cmd, cwd, port, childHost)
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
		return applyRunOptions(cmd, RunPlan{Port: port, Host: host, Name: strings.TrimSuffix(host, ".localhost"), CWD: cwd, ProjectRoot: cwd, ProjectName: project.NormalizeHostnameName(filepath.Base(cwd)), Static: true})
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
			return applyRunOptions(cmd, RunPlan{Port: port, Host: host, Name: strings.TrimSuffix(host, ".localhost"), CWD: cwd, ProjectRoot: cwd, ProjectName: project.NormalizeHostnameName(filepath.Base(cwd)), Static: true})
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
	injected := runner.InjectPortArgsForHost(scriptCommand, port, cmd.PortFlag, childHost)
	command := runner.BuildScriptCommand(pm, cmd.Script, injected)
	managedPort := len(injected) > 0
	host, err := project.HostnameForProject(cwd)
	if err != nil {
		return RunPlan{}, err
	}
	projectRoot := projectDir(packagePath)
	projectName, err := project.ProjectNameForRoot(projectRoot)
	if err != nil {
		return RunPlan{}, err
	}
	return applyRunOptions(cmd, RunPlan{Command: command, Env: env, Port: port, Host: host, Name: strings.TrimSuffix(host, ".localhost"), CWD: cwd, ProjectRoot: projectRoot, ProjectName: projectName, ManagedPort: managedPort})
}

func applyRunOptions(cmd cli.Command, plan RunPlan) (RunPlan, error) {
	plan, err := applyAsAlias(cmd, plan)
	if err != nil {
		return RunPlan{}, err
	}
	if cmd.Live {
		if !plan.Static {
			return RunPlan{}, liveStaticOnlyError()
		}
		plan.Live = true
	}
	return plan, nil
}

func applyAsAlias(cmd cli.Command, plan RunPlan) (RunPlan, error) {
	if cmd.As == "" {
		return plan, nil
	}
	name, ok := project.NormalizeHostnameAlias(cmd.As)
	if !ok {
		return RunPlan{}, fmt.Errorf("Invalid alias: %s\nAliases can contain letters, numbers, spaces, dots, underscores, and hyphens.", cmd.As)
	}
	plan.Name = name
	plan.Host = name + ".localhost"
	return plan, nil
}

func Run(ctx context.Context, cmd cli.Command, cwd string, stdout, stderr io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if cmd.Kind == cli.CommandRun && cmd.TargetPath != "" {
		targetPath, info, err := resolvePathTarget(cwd, cmd.TargetPath)
		if err != nil {
			return err
		}
		if info.IsDir() {
			workspaceCmd := cmd
			workspaceCmd.TargetPath = ""
			workspaceCmd.Script = "dev"
			workspaceCmd.Scripts = nil
			workspaceCmd.ExplicitScript = false
			if shouldRunWorkspace(workspaceCmd) {
				ran, err := runWorkspaceIfAvailable(ctx, workspaceCmd, targetPath, stdout, stderr)
				if err != nil || ran {
					return err
				}
			}
		}
	}
	if shouldRunWorkspace(cmd) {
		ran, err := runWorkspaceIfAvailable(ctx, cmd, cwd, stdout, stderr)
		if err != nil || ran {
			return err
		}
	}
	if cmd.Kind == cli.CommandRun && len(cmd.Scripts) > 1 {
		return runMulti(ctx, cmd, cwd, stdout, stderr)
	}

	var adminClient adminClient
	routerResolved := false
	var resolvedRouter runRouter
	var plan RunPlan
	ensureRunRouter := func() error {
		if routerResolved {
			return nil
		}
		rr, err := resolveRunRouter(ctx, stderr)
		if err != nil {
			return err
		}
		adminClient = rr.Client
		applyRunRouter(&plan, rr)
		resolvedRouter = rr
		routerResolved = true
		return nil
	}

	var err error
	plan, err = PrepareRun(cmd, cwd)
	if err != nil {
		return err
	}

	if detectWSLFunc() {
		if err := ensureRunRouter(); err != nil {
			return err
		}
		if shouldPrepareForChildBindHost(cmd, resolvedRouter.ChildHost) {
			plan, err = prepareRunWithHost(cmd, cwd, resolvedRouter.ChildHost)
			if err != nil {
				return err
			}
			applyRunRouter(&plan, resolvedRouter)
		}
	}

	if plan.Static {
		if err := ensureRunRouter(); err != nil {
			return err
		}

		staticServer, err := staticserver.StartWithConfig(ctx, staticserver.Config{
			Dir:  plan.CWD,
			Port: plan.Port,
			Host: plan.StaticBindHost,
			Live: plan.Live,
		})
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
		Dir:                 runnerDirForRun(cmd, plan),
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
		if errors.Is(err, runner.ErrProcessFinished) {
			fmt.Fprint(stdout, runFinishedOutput(cmd))
			return nil
		}
		return formatRunError(cmd, err)
	}
	defer result.Stop()

	if err := ensureRunRouter(); err != nil {
		return err
	}

	cleanup, err := registerRoute(ctx, adminClient, cmd, plan, result.Port, result.PID(), stdout, stderr)
	if err != nil {
		return err
	}
	defer cleanup()
	return result.Wait()
}

func runnerDirForRun(cmd cli.Command, plan RunPlan) string {
	if cmd.Kind == cli.CommandRun && cmd.TargetPath != "" {
		return plan.CWD
	}
	return ""
}

func shouldRunWorkspace(cmd cli.Command) bool {
	return cmd.Kind == cli.CommandRun &&
		cmd.Script == "dev" &&
		len(cmd.Scripts) == 0 &&
		!cmd.ExplicitScript &&
		!cmd.Live &&
		cmd.As == "" &&
		cmd.TargetPort == 0
}

func runWorkspaceIfAvailable(ctx context.Context, cmd cli.Command, cwd string, stdout, stderr io.Writer) (bool, error) {
	packages, found, err := project.DiscoverWorkspacePackages(cwd, cmd.Script)
	if err != nil || !found {
		return false, err
	}
	if len(packages) == 0 {
		return true, noWorkspacePackageScriptError(cmd.Script)
	}

	pm, _, err := project.DetectPackageManager(cwd)
	if err != nil {
		return true, err
	}

	return true, runWorkspace(ctx, cmd, cwd, pm, packages, stdout, stderr)
}

func noWorkspacePackageScriptError(script string) error {
	return fmt.Errorf("gohere error: No workspace packages with a %q script found.\nRun `gohere %s` to run the current package script exactly as written.", script, script)
}

func runWorkspace(ctx context.Context, cmd cli.Command, root string, pm project.PackageManager, packages []project.WorkspacePackage, stdout, stderr io.Writer) error {
	var items []multiRunItem
	cleanupStarted := func() {
		for i := len(items) - 1; i >= 0; i-- {
			if items[i].cleanup != nil {
				items[i].cleanup()
			}
			if items[i].result != nil {
				items[i].result.Stop()
			}
		}
	}
	defer cleanupStarted()

	var adminClient adminClient
	routerResolved := false
	ensureRunRouter := func() (runRouter, error) {
		if routerResolved {
			return runRouter{Client: adminClient}, nil
		}
		rr, err := resolveRunRouter(ctx, stderr)
		if err != nil {
			return runRouter{}, err
		}
		adminClient = rr.Client
		routerResolved = true
		return rr, nil
	}

	var resolvedRouter runRouter
	prepareHost := "127.0.0.1"
	if detectWSLFunc() {
		rr, err := ensureRunRouter()
		if err != nil {
			return err
		}
		resolvedRouter = rr
		if rr.ChildHost != "" {
			prepareHost = rr.ChildHost
		}
	}

	for _, workspacePackage := range packages {
		plan, err := prepareWorkspacePackageRun(cmd, root, pm, workspacePackage, prepareHost)
		if err != nil {
			return err
		}
		if detectWSLFunc() {
			applyRunRouter(&plan, resolvedRouter)
		}
		itemCmd := cmd
		itemCmd.Script = workspacePackage.ShortName
		itemCmd.Scripts = nil
		itemCmd.ExplicitScript = true
		items = append(items, multiRunItem{cmd: itemCmd, plan: plan})
	}
	if err := applyServiceDiscoveryEnv(items); err != nil {
		return err
	}

	for i := range items {
		itemCmd := items[i].cmd
		plan := items[i].plan
		if routerResolved {
			applyRunRouter(&plan, resolvedRouter)
		}
		childStdout := newLimitedCapture(32 * 1024)
		childStderr := newLimitedCapture(32 * 1024)
		result, err := startRunnerFunc(ctx, runner.Config{
			Command:        plan.Command,
			Dir:            plan.CWD,
			Env:            plan.Env,
			ChosenPort:     plan.Port,
			Stdout:         childStdout,
			Stderr:         childStderr,
			StartupTimeout: 15 * time.Second,
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			replayCapturedOutput(stderr, childStdout, childStderr)
			return formatMultiRunError(itemCmd, err)
		}

		if !routerResolved {
			rr, err := ensureRunRouter()
			if err != nil {
				result.Stop()
				return err
			}
			resolvedRouter = rr
			applyRunRouter(&plan, rr)
		}

		cleanup, err := registerRoute(ctx, adminClient, itemCmd, plan, result.Port, result.PID(), stdout, stderr)
		if err != nil {
			result.Stop()
			return err
		}
		items[i].plan = plan
		items[i].result = result
		items[i].cleanup = cleanup
	}

	return waitForMulti(ctx, items)
}

func prepareWorkspacePackageRun(cmd cli.Command, root string, pm project.PackageManager, workspacePackage project.WorkspacePackage, childHost string) (RunPlan, error) {
	port, err := chooseFreePortForHostFunc(childHost)
	if err != nil {
		return RunPlan{}, err
	}
	env := runner.ChildEnvForHost(os.Environ(), port, childHost)
	injected := runner.InjectPortArgsForHost(workspacePackage.Script, port, cmd.PortFlag, childHost)
	command := runner.BuildScriptCommand(pm, cmd.Script, injected)
	managedPort := len(injected) > 0
	host, err := project.HostnameForProject(workspacePackage.Dir)
	if err != nil {
		return RunPlan{}, err
	}
	projectName, err := project.ProjectNameForRoot(root)
	if err != nil {
		return RunPlan{}, err
	}
	return RunPlan{
		Command:     command,
		Env:         env,
		Port:        port,
		Host:        host,
		Name:        strings.TrimSuffix(host, ".localhost"),
		CWD:         workspacePackage.Dir,
		ProjectRoot: root,
		ProjectName: projectName,
		ManagedPort: managedPort,
	}, nil
}

type multiRunItem struct {
	cmd     cli.Command
	plan    RunPlan
	result  *runner.Result
	cleanup func()
}

func runMulti(ctx context.Context, cmd cli.Command, cwd string, stdout, stderr io.Writer) error {
	var items []multiRunItem
	cleanupStarted := func() {
		for i := len(items) - 1; i >= 0; i-- {
			if items[i].cleanup != nil {
				items[i].cleanup()
			}
			if items[i].result != nil {
				items[i].result.Stop()
			}
		}
	}
	defer cleanupStarted()

	var adminClient adminClient
	routerResolved := false
	ensureRunRouter := func() (runRouter, error) {
		if routerResolved {
			return runRouter{Client: adminClient}, nil
		}
		rr, err := resolveRunRouter(ctx, stderr)
		if err != nil {
			return runRouter{}, err
		}
		adminClient = rr.Client
		routerResolved = true
		return rr, nil
	}

	var resolvedRouter runRouter
	prepareHost := "127.0.0.1"
	if detectWSLFunc() {
		rr, err := ensureRunRouter()
		if err != nil {
			return err
		}
		resolvedRouter = rr
		if rr.ChildHost != "" {
			prepareHost = rr.ChildHost
		}
	}

	for _, script := range cmd.Scripts {
		itemCmd := cmd
		itemCmd.Script = script
		itemCmd.Scripts = nil
		plan, err := prepareRunWithHost(itemCmd, cwd, prepareHost)
		if err != nil {
			return err
		}
		if plan.Static {
			return errors.New("gohere error: multiple projects only support package scripts")
		}
		plan.Host = multiScriptHost(script, plan.Host)
		plan.Name = strings.TrimSuffix(plan.Host, ".localhost")
		if detectWSLFunc() {
			applyRunRouter(&plan, resolvedRouter)
		}
		items = append(items, multiRunItem{cmd: itemCmd, plan: plan})
	}
	if err := applyServiceDiscoveryEnv(items); err != nil {
		return err
	}

	for i := range items {
		itemCmd := items[i].cmd
		plan := items[i].plan
		if routerResolved {
			applyRunRouter(&plan, resolvedRouter)
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
			return formatMultiRunError(itemCmd, err)
		}

		if !routerResolved {
			rr, err := ensureRunRouter()
			if err != nil {
				result.Stop()
				return err
			}
			resolvedRouter = rr
			applyRunRouter(&plan, rr)
		}

		cleanup, err := registerRoute(ctx, adminClient, itemCmd, plan, result.Port, result.PID(), stdout, stderr)
		if err != nil {
			result.Stop()
			return err
		}
		items[i].plan = plan
		items[i].result = result
		items[i].cleanup = cleanup
	}

	return waitForMulti(ctx, items)
}

func multiScriptHost(script, baseHost string) string {
	base := strings.TrimSuffix(baseHost, ".localhost")
	label := script
	if index := strings.LastIndex(script, ":"); index >= 0 && index < len(script)-1 {
		label = script[index+1:]
	}
	return project.NormalizeHostnameName(label) + "." + base + ".localhost"
}

type serviceDiscoveryEntry struct {
	Key     string `json:"key"`
	URL     string `json:"url"`
	Port    int    `json:"port,omitempty"`
	Target  string `json:"target,omitempty"`
	Managed bool   `json:"managed"`
}

func applyServiceDiscoveryEnv(items []multiRunItem) error {
	if len(items) <= 1 {
		return nil
	}
	values, err := serviceDiscoveryEnv(items)
	if err != nil {
		return err
	}
	for i := range items {
		items[i].plan.Env = appendMissingEnv(items[i].plan.Env, values)
	}
	return nil
}

func serviceDiscoveryEnv(items []multiRunItem) (map[string]string, error) {
	values := map[string]string{}
	services := map[string]serviceDiscoveryEntry{}
	seen := map[string]string{}

	for _, item := range items {
		label := serviceDiscoveryLabel(item)
		key := serviceDiscoveryEnvKey(label)
		if key == "" {
			return nil, fmt.Errorf("gohere error: service env key is empty for %s", serviceDiscoverySource(item))
		}
		if existing, ok := seen[key]; ok {
			return nil, fmt.Errorf("gohere error: service env key %q is ambiguous for %s and %s", key, existing, serviceDiscoverySource(item))
		}
		seen[key] = serviceDiscoverySource(item)

		url := publicRouteURL(item.plan.Host, item.plan.URLPath)
		entry := serviceDiscoveryEntry{
			Key:     key,
			URL:     url,
			Managed: item.plan.ManagedPort,
		}
		values["GOHERE_"+key+"_URL"] = url
		if item.plan.ManagedPort {
			portValue := fmt.Sprintf("%d", item.plan.Port)
			target := fmt.Sprintf("http://127.0.0.1:%d", item.plan.Port)
			entry.Port = item.plan.Port
			entry.Target = target
			values["GOHERE_"+key+"_PORT"] = portValue
			values["GOHERE_"+key+"_TARGET"] = target
		}
		services[strings.ToLower(key)] = entry
	}

	data, err := json.Marshal(services)
	if err != nil {
		return nil, err
	}
	values["GOHERE_SERVICES_JSON"] = string(data)
	return values, nil
}

func serviceDiscoveryLabel(item multiRunItem) string {
	host := strings.TrimSuffix(item.plan.Host, ".localhost")
	if label, _, ok := strings.Cut(host, "."); ok {
		return label
	}
	if host != "" {
		return host
	}
	name := runName(item.cmd)
	if index := strings.LastIndex(name, ":"); index >= 0 && index < len(name)-1 {
		name = name[index+1:]
	}
	return project.NormalizeHostnameName(name)
}

func serviceDiscoveryEnvKey(label string) string {
	var out strings.Builder
	lastUnderscore := false
	for _, r := range strings.ToUpper(label) {
		switch {
		case r >= 'A' && r <= 'Z' || r >= '0' && r <= '9':
			out.WriteRune(r)
			lastUnderscore = false
		default:
			if !lastUnderscore {
				out.WriteByte('_')
				lastUnderscore = true
			}
		}
	}
	return strings.Trim(out.String(), "_")
}

func serviceDiscoverySource(item multiRunItem) string {
	if item.cmd.Script != "" {
		return item.cmd.Script
	}
	if item.plan.CWD != "" {
		return item.plan.CWD
	}
	if item.plan.Host != "" {
		return item.plan.Host
	}
	return "unknown service"
}

func appendMissingEnv(env []string, values map[string]string) []string {
	if len(values) == 0 {
		return env
	}
	seen := map[string]bool{}
	for _, item := range env {
		key, _, ok := strings.Cut(item, "=")
		if ok {
			seen[key] = true
		}
	}

	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	out := append([]string(nil), env...)
	for _, key := range keys {
		if !seen[key] {
			out = append(out, key+"="+values[key])
		}
	}
	return out
}

func formatMultiRunError(cmd cli.Command, err error) error {
	if errors.Is(err, runner.ErrProcessFinished) {
		return fmt.Errorf("gohere error: script %q finished without starting a local server.", runName(cmd))
	}
	return formatRunError(cmd, err)
}

func waitForMulti(ctx context.Context, items []multiRunItem) error {
	if len(items) == 0 {
		return nil
	}
	type waitResult struct {
		cmd cli.Command
		err error
	}
	done := make(chan waitResult, len(items))
	for _, item := range items {
		cmd := item.cmd
		result := item.result
		go func() {
			done <- waitResult{cmd: cmd, err: result.Wait()}
		}()
	}
	select {
	case result := <-done:
		if result.err != nil {
			return formatMultiRunError(result.cmd, errors.Join(runner.ErrProcessFailed, result.err))
		}
		return nil
	case <-ctx.Done():
		for _, item := range items {
			if item.result != nil {
				item.result.Stop()
			}
		}
		for range items {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				return nil
			}
		}
		return nil
	}
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
	processIdentity, _ := lifecycle.ProcessIdentity(pid)
	ownerEnv := plan.OwnerEnv
	if ownerEnv == "" {
		ownerEnv = runOwnerEnv()
	}
	route := router.Route{
		Host:            plan.Host,
		Target:          routeTarget(plan.RouteTargetHost, port),
		PID:             pid,
		CWD:             plan.CWD,
		Name:            plan.Name,
		Mode:            runMode(cmd, plan),
		ProjectRoot:     plan.ProjectRoot,
		ProjectName:     plan.ProjectName,
		Source:          plan.RouteSource,
		OwnerCWD:        plan.CWD,
		OwnerEnv:        ownerEnv,
		StartedAt:       time.Now().UTC(),
		ProcessIdentity: processIdentity,
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
		fmt.Fprintf(stdout, "service: %s\n", routerLabel)
	}
	return func() {
		adminClient.DeleteRoute(context.Background(), route.Host)
	}, nil
}

func runMode(cmd cli.Command, plan RunPlan) string {
	if plan.Mode != "" {
		return plan.Mode
	}
	if plan.Static {
		return "static"
	}
	if cmd.Kind == cli.CommandRaw {
		return "raw"
	}
	return "package"
}

func runOwnerEnv() string {
	if detectWSLFunc() {
		return "wsl"
	}
	return runtime.GOOS
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
				return runRouter{}, localRouterControlError(runtime.GOOS, router.DefaultStateDir())
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

	token, tokenPath, err := discoverWindowsTokenFunc(windowsUsersRoot)
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
		if err := startWindowsServiceFunc(ctx, tokenPath); err != nil {
			return runRouter{}, windowsRouterUnavailableError(err)
		}
		if err := waitForRouterHealth(ctx, windowsRouterHealthFunc, routerStartTimeout); err != nil {
			return runRouter{}, windowsRouterUnavailableError(err)
		}
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
		ChildHost:       bridgeChildHost(targetHost),
		RouteSource:     "wsl",
		OwnerEnv:        "wsl",
		RouterLabel:     "Windows",
		Bridge:          true,
	}, nil
}

func bridgeTargetHost(ctx context.Context, client bridgeProbeClient, wslIP string) (string, error) {
	candidates := bridgeTargetCandidates(wslIP)
	for _, candidate := range candidates {
		reachable, _, err := probeBridgeFunc(ctx, client, candidate)
		if err != nil {
			return "", windowsRouterCannotReachWSLError(err)
		}
		if reachable {
			return candidate, nil
		}
	}
	return "", windowsRouterCannotReachWSLError(fmt.Errorf("Tried: %s", strings.Join(candidates, ", ")))
}

func bridgeTargetCandidates(wslIP string) []string {
	candidates := []string{"127.0.0.1", "localhost"}
	if wslIP != "" && wslIP != "127.0.0.1" && wslIP != "localhost" {
		candidates = append(candidates, wslIP)
	}
	return candidates
}

func bridgeChildHost(targetHost string) string {
	if targetHost == "127.0.0.1" || strings.EqualFold(targetHost, "localhost") {
		return "127.0.0.1"
	}
	return "0.0.0.0"
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
		plan.Env = runner.ChildEnvForHost(plan.Env, plan.Port, rr.ChildHost)
		plan.Command = withHost(plan.Command, rr.ChildHost)
	}
}

func shouldPrepareForChildBindHost(cmd cli.Command, childHost string) bool {
	if cmd.TargetPort != 0 {
		return false
	}
	return childHost != "" && childHost != "127.0.0.1" && !strings.EqualFold(childHost, "localhost")
}

func withHost(command []string, host string) []string {
	if host == "" || host == "127.0.0.1" {
		return command
	}
	out := append([]string(nil), command...)
	rewriteNext := false
	for i := range out {
		if rewriteNext {
			if isCommonLoopbackHost(out[i]) {
				out[i] = host
			}
			rewriteNext = false
			continue
		}
		if isSeparateHostFlag(out[i]) {
			rewriteNext = true
			continue
		}
		out[i] = replaceCommandHost(out[i], host)
	}
	return out
}

func replaceCommandHost(arg, host string) string {
	for _, prefix := range []string{"--host=", "--hostname=", "--listen=", "--allowed-hosts="} {
		if strings.HasPrefix(arg, prefix) && isCommonLoopbackHost(strings.TrimPrefix(arg, prefix)) {
			return prefix + host
		}
	}
	return arg
}

func isSeparateHostFlag(flag string) bool {
	switch flag {
	case "--host", "--hostname", "--listen", "--allowed-hosts":
		return true
	default:
		return false
	}
}

func isCommonLoopbackHost(host string) bool {
	return host == "127.0.0.1" || host == "localhost" || host == "0.0.0.0"
}

func routeTarget(host string, port int) string {
	if host == "" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, fmt.Sprint(port))
}

func windowsTokenError(err error) error {
	return errors.New("Windows gohere service is available, but WSL could not use it.\n\nWhen Windows and WSL are both installed, WSL projects should use the Windows service.\n\nRun:\n  gohere doctor")
}

func startWindowsService(ctx context.Context, tokenPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, windowsServiceStartTimeout)
	defer cancel()

	stableBinary := filepath.Join(filepath.Dir(tokenPath), "bin", "gohere.exe")
	if !exists(stableBinary) {
		return fmt.Errorf("%s: %w", stableBinary, os.ErrNotExist)
	}
	output, err := execCommandContext(ctx, "wslpath", "-w", stableBinary).CombinedOutput()
	if err != nil {
		return commandOutputError("wslpath", output, err)
	}
	windowsBinary := strings.TrimSpace(string(output))
	command := "Start-Process -WindowStyle Hidden -FilePath " + powerShellQuote(windowsBinary) + " -ArgumentList @('service','run')"
	output, err = execCommandContext(ctx, "powershell.exe", "-NoProfile", "-Command", command).CombinedOutput()
	if err != nil {
		return commandOutputError("powershell.exe", output, err)
	}
	return nil
}

func commandOutputError(command string, output []byte, err error) error {
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		return fmt.Errorf("%s failed: %w", command, err)
	}
	return fmt.Errorf("%s failed: %w: %s", command, err, detail)
}

func powerShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func windowsRouterUnavailableError(cause error) error {
	msg := "Windows gohere is installed, but WSL could not start its service.\n\nRun this from Windows:\n  gohere\n\nThen run gohere again from WSL."
	if cause != nil {
		return fmt.Errorf("%s\n\nDetails: %w", msg, cause)
	}
	return errors.New(msg)
}

func windowsRouterCannotReachWSLError(cause error) error {
	msg := "Windows gohere service is running, but cannot reach WSL dev servers.\n\nThis can happen if WSL networking is not mirrored, Windows Firewall blocks the probe, or WSL networking is still starting.\n\nTry enabling mirrored networking in %USERPROFILE%\\.wslconfig:\n  [wsl2]\n  networkingMode=mirrored\nThen run:\n  wsl --shutdown"
	if cause != nil {
		return fmt.Errorf("%s\n\nDetails: %w", msg, cause)
	}
	return errors.New(msg)
}

func staleRouterTokenError() error {
	return errors.New("A gohere service is already using .localhost URLs, but this install cannot control it.\n\nWhen using Windows and WSL together, the Windows service should own .localhost URLs.\nWSL projects will register with the Windows service.\n\nIn the other environment, run:\n  gohere service stop\n\nThen run gohere again.")
}

func localRouterControlError(goos, stateDir string) error {
	if goos == "windows" {
		stableBinary := filepath.Join(stateDir, "bin", stableBinaryName(goos))
		tokenPath := filepath.Join(stateDir, "token")
		if !exists(stableBinary) || !exists(tokenPath) {
			return errors.New("A WSL gohere service is using .localhost URLs.\n\nWhen using Windows and WSL together, the Windows service should own .localhost URLs.\nWSL projects will register with the Windows service.\n\nIn WSL, run:\n  gohere service stop\n\nThen run gohere again from Windows.")
		}
	}
	return staleRouterTokenError()
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
	if cmd.Kind == cli.CommandRun {
		switch {
		case cmd.TargetPath != "":
			label += " " + cmd.TargetPath
		case cmd.Script != "" && (cmd.Script != "dev" || urlPath != ""):
			label += " " + cmd.Script
		}
	}
	return fmt.Sprintf("%s \u2192 http://%s%s\n", label, host, escapedURLPath(urlPath))
}

func publicRouteURL(host, urlPath string) string {
	return fmt.Sprintf("http://%s%s", host, escapedURLPath(urlPath))
}

func isFileTarget(cmd cli.Command) bool {
	return cmd.Kind == cli.CommandRun && cmd.Script != "" && filepath.Ext(cmd.Script) != ""
}

func preparePathTarget(cmd cli.Command, cwd string, port int, childHost string) (RunPlan, error) {
	targetPath, info, err := resolvePathTarget(cwd, cmd.TargetPath)
	if err != nil {
		return RunPlan{}, err
	}
	if !info.IsDir() {
		return prepareStaticFilePathTarget(cmd, targetPath, port)
	}

	targetCmd := cmd
	targetCmd.TargetPath = ""
	targetCmd.Script = "dev"
	targetCmd.Scripts = nil
	targetCmd.ExplicitScript = false
	targetCmd.TargetPort = port
	return prepareRunWithHost(targetCmd, targetPath, childHost)
}

func resolvePathTarget(cwd, input string) (string, os.FileInfo, error) {
	cleanPath := filepath.Clean(input)
	if !filepath.IsAbs(cleanPath) {
		cleanPath = filepath.Join(cwd, cleanPath)
	}
	targetPath, err := filepath.Abs(cleanPath)
	if err != nil {
		return "", nil, err
	}
	info, err := os.Stat(targetPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil, fmt.Errorf("path not found: %s", input)
		}
		return "", nil, err
	}
	return targetPath, info, nil
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

	return prepareStaticFilePlan(cmd, cwd, "/"+filepath.ToSlash(cleanPath), port)
}

func prepareStaticFilePathTarget(cmd cli.Command, targetPath string, port int) (RunPlan, error) {
	staticRoot := filepath.Dir(targetPath)
	return prepareStaticFilePlan(cmd, staticRoot, "/"+filepath.ToSlash(filepath.Base(targetPath)), port)
}

func prepareStaticFilePlan(cmd cli.Command, root, urlPath string, port int) (RunPlan, error) {
	host := project.NormalizeHostnameName(filepath.Base(root)) + ".localhost"
	return applyRunOptions(cmd, RunPlan{
		Port:        port,
		Host:        host,
		Name:        strings.TrimSuffix(host, ".localhost"),
		CWD:         root,
		ProjectRoot: root,
		ProjectName: project.NormalizeHostnameName(filepath.Base(root)),
		Static:      true,
		URLPath:     urlPath,
	})
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

func liveStaticOnlyError() error {
	return errors.New("gohere error: --live is only supported for static files and folders.")
}

func formatRunError(cmd cli.Command, err error) error {
	if errors.Is(err, runner.ErrProcessFailed) {
		if cmd.Kind == cli.CommandRun {
			return fmt.Errorf("gohere error: script %q failed.", runName(cmd))
		}
		return errors.New("gohere error: command failed.")
	}
	if errors.Is(err, runner.ErrNoLocalURL) || strings.Contains(err.Error(), "could not detect a local URL") {
		name := runName(cmd)
		return fmt.Errorf("gohere error: started %q, but no local URL was detected.\nTry:\n  gohere --target 5173 %s", name, name)
	}
	return fmt.Errorf("gohere error: %w", err)
}

func runFinishedOutput(cmd cli.Command) string {
	if cmd.Kind == cli.CommandRaw {
		return "gohere command finished.\n"
	}
	return fmt.Sprintf("gohere %s finished.\n", runName(cmd))
}

func runName(cmd cli.Command) string {
	if cmd.Kind == cli.CommandRun && cmd.TargetPath != "" {
		return cmd.TargetPath
	}
	if cmd.Kind == cli.CommandRun && cmd.Script != "" {
		return cmd.Script
	}
	return "command"
}

func replayCapturedOutput(out io.Writer, captures ...*limitedCapture) {
	wrote := false
	lastEndedNewline := true
	for _, capture := range captures {
		text := capture.String()
		if text == "" {
			continue
		}
		if wrote && !lastEndedNewline {
			fmt.Fprintln(out)
		}
		fmt.Fprint(out, text)
		lastEndedNewline = strings.HasSuffix(text, "\n")
		wrote = true
	}
	if wrote && !lastEndedNewline {
		fmt.Fprintln(out)
	}
}

type limitedCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func newLimitedCapture(max int) *limitedCapture {
	return &limitedCapture{max: max}
}

func (w *limitedCapture) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
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
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func ensureRouter(ctx context.Context, out io.Writer, health func(context.Context) error) error {
	if err := health(ctx); err == nil {
		return nil
	}
	if err := startInstalledRouterFunc(ctx); err == nil {
		if err := waitForRouterHealth(ctx, health, routerStartTimeout); err == nil {
			return nil
		} else {
			return installedRouterUnavailableError(err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return installedRouterUnavailableError(err)
	}

	fmt.Fprint(out, firstRunPrompt())
	answer, readErr := bufio.NewReader(promptInput).ReadString('\n')
	if readErr != nil && strings.TrimSpace(answer) == "" {
		fmt.Fprint(out, "gohere was not enabled.\n\nRun gohere again when you are ready.\n")
		return errors.New("gohere was not enabled")
	}
	if !shouldRunSetupFromAnswer(answer) {
		fmt.Fprint(out, "gohere was not enabled.\n\nRun gohere again when you are ready.\n")
		return errors.New("gohere was not enabled")
	}
	if err := setupFunc(ctx); err != nil {
		return err
	}
	if err := waitForRouterHealth(ctx, health, routerStartTimeout); err != nil {
		return fmt.Errorf("gohere setup finished, but the service is still not reachable: %w", err)
	}
	fmt.Fprintln(out)
	return nil
}

func installedRouterUnavailableError(err error) error {
	return fmt.Errorf("installed gohere service is not reachable.\nTry:\n  gohere doctor\n\nDetails: %w", err)
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
	return firstRunPromptForGOOS(runtime.GOOS)
}

func firstRunPromptForGOOS(goos string) string {
	if goos == "linux" {
		return "gohere needs one-time permission to enable .localhost project URLs.\nThis lets gohere use port 80 locally, and sudo access may be requested. Continue? [Y/n] "
	}
	return "gohere needs one-time permission to enable .localhost project URLs.\nThis lets gohere use port 80 locally. Continue? [Y/n] "
}

func shouldRunSetupFromAnswer(answer string) bool {
	answer = strings.TrimSpace(strings.ToLower(answer))
	return answer == "" || strings.HasPrefix(answer, "y")
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
	case "darwin":
		return setupDarwinFunc(ctx, cfg)
	default:
		return fmt.Errorf("gohere setup is not supported on %s yet", goos)
	}
}

type ListOptions struct {
	Verbose bool
	JSON    bool
}

type listRoute struct {
	Host        string `json:"host"`
	Target      string `json:"target"`
	Status      string `json:"status"`
	PID         int    `json:"pid"`
	CWD         string `json:"cwd"`
	Name        string `json:"name,omitempty"`
	Mode        string `json:"mode"`
	Source      string `json:"source"`
	OwnerEnv    string `json:"ownerEnv"`
	ProjectRoot string `json:"projectRoot,omitempty"`
	ProjectName string `json:"projectName,omitempty"`
	StartedAt   string `json:"startedAt,omitempty"`
	CanStop     bool   `json:"canStop"`
	StopReason  string `json:"stopReason,omitempty"`
}

func List(ctx context.Context, stdout io.Writer, verbose bool) error {
	return ListWithOptions(ctx, stdout, ListOptions{Verbose: verbose})
}

func ListWithOptions(ctx context.Context, stdout io.Writer, opts ListOptions) error {
	manager, err := resolveRouteManager(ctx)
	if err != nil {
		return err
	}
	if manager.Client != nil {
		statuses, err := adminRouteStatuses(ctx, manager.Client)
		if err != nil {
			if errors.Is(err, admin.ErrUnauthorized) {
				return staleRouterTokenError()
			}
			return err
		}
		return printRouteStatuses(stdout, statuses, opts)
	}
	return ListWithStoreRouterReadyOptions(stdout, manager.Store, opts, manager.RouterReady)
}

func ListWithStore(stdout io.Writer, store router.Store, verbose bool) error {
	return ListWithStoreOptions(stdout, store, ListOptions{Verbose: verbose})
}

func ListWithStoreOptions(stdout io.Writer, store router.Store, opts ListOptions) error {
	return ListWithStoreRouterReadyOptions(stdout, store, opts, true)
}

func ListWithStoreRouterReady(stdout io.Writer, store router.Store, verbose bool, routerReady bool) error {
	return ListWithStoreRouterReadyOptions(stdout, store, ListOptions{Verbose: verbose}, routerReady)
}

func ListWithStoreRouterReadyOptions(stdout io.Writer, store router.Store, opts ListOptions, routerReady bool) error {
	routes, err := store.Load()
	if err != nil {
		return err
	}
	return printRoutesWithRouterReady(stdout, routes, opts, routerReady)
}

func printRoutesWithRouterReady(stdout io.Writer, routes []router.Route, opts ListOptions, routerReady bool) error {
	statuses := lifecycle.RouteStatusesWithRouterReady(routes, routerReady)
	return printRouteStatuses(stdout, statuses, opts)
}

func printRouteStatuses(stdout io.Writer, statuses []lifecycle.RouteStatus, opts ListOptions) error {
	if opts.JSON {
		return printRouteStatusesJSON(stdout, statuses)
	}
	if opts.Verbose {
		fmt.Fprint(stdout, lifecycle.FormatRoutesVerbose(statuses))
		return nil
	}
	fmt.Fprint(stdout, lifecycle.FormatRoutes(statuses))
	return nil
}

func printRouteStatusesJSON(stdout io.Writer, statuses []lifecycle.RouteStatus) error {
	routes := make([]listRoute, 0, len(statuses))
	for _, status := range statuses {
		canStop, stopReason := lifecycle.RouteStopInfo(status)
		route := status.Route
		routes = append(routes, listRoute{
			Host:        route.Host,
			Target:      route.Target,
			Status:      string(status.Status),
			PID:         route.PID,
			CWD:         route.CWD,
			Name:        route.Name,
			Mode:        lifecycle.RouteMode(route),
			Source:      lifecycle.RouteSource(route),
			OwnerEnv:    lifecycle.RouteOwner(route),
			ProjectRoot: route.ProjectRoot,
			ProjectName: route.ProjectName,
			StartedAt:   startedAtJSON(route),
			CanStop:     canStop,
			StopReason:  stopReason,
		})
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(routes)
}

func startedAtJSON(route router.Route) string {
	if route.StartedAt.IsZero() {
		return ""
	}
	return route.StartedAt.UTC().Format(time.RFC3339)
}

func Prune(ctx context.Context, stdout io.Writer) error {
	manager, err := resolveRouteManager(ctx)
	if err != nil {
		return err
	}
	if manager.Client != nil {
		removed, err := pruneAdminRoutes(ctx, manager.Client)
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

func Stop(ctx context.Context, cwd string, stdout io.Writer) error {
	return StopWithCommand(ctx, cli.Command{Kind: cli.CommandStop}, cwd, stdout)
}

func StopWithCommand(ctx context.Context, cmd cli.Command, cwd string, stdout io.Writer) error {
	manager, err := resolveRouteManager(ctx)
	if err != nil {
		return err
	}
	if cmd.StopAll || cmd.StopTarget != "" {
		return stopExplicitRoutes(ctx, manager, cmd, stdout)
	}
	cwds, err := stopCandidateCWDs(cwd)
	if err != nil {
		return err
	}
	var result lifecycle.StopResult
	if manager.Client != nil {
		result, err = stopAdminCWDs(ctx, manager.Client, cwds)
	} else {
		result, err = lifecycle.StopCWDs(manager.Store, cwds)
	}
	if err != nil {
		if result.MatchedHost != "" {
			return fmt.Errorf("gohere error: could not stop %s: %w\nTry:\n  gohere doctor", result.MatchedHost, err)
		}
		return fmt.Errorf("gohere error: %w", err)
	}
	printStopResults(stdout, result)
	return nil
}

func stopExplicitRoutes(ctx context.Context, manager routeManager, cmd cli.Command, stdout io.Writer) error {
	routes, err := routeManagerRoutes(ctx, manager)
	if err != nil {
		return err
	}
	var selected []router.Route
	if cmd.StopAll {
		selected = routes
		if len(selected) == 0 {
			fmt.Fprintln(stdout, "No active routes.")
			return nil
		}
	} else {
		selected, err = resolveStopTarget(routes, cmd.StopTarget)
		if err != nil {
			return err
		}
		if len(selected) == 0 {
			fmt.Fprintln(stdout, "No matching gohere route found.")
			return nil
		}
	}
	var result lifecycle.StopResult
	if manager.Client != nil {
		result, err = stopAdminRoutes(ctx, manager.Client, selected)
	} else {
		result, err = stopStoreRoutes(manager.Store, selected)
	}
	if err != nil {
		if result.MatchedHost != "" {
			return fmt.Errorf("gohere error: could not stop %s: %w\nTry:\n  gohere doctor", result.MatchedHost, err)
		}
		return fmt.Errorf("gohere error: %w", err)
	}
	printStopResults(stdout, result)
	return nil
}

func routeManagerRoutes(ctx context.Context, manager routeManager) ([]router.Route, error) {
	if manager.Client != nil {
		routes, err := manager.Client.Routes(ctx)
		if err != nil {
			if errors.Is(err, admin.ErrUnauthorized) {
				return nil, staleRouterTokenError()
			}
			return nil, err
		}
		return routes, nil
	}
	return manager.Store.Load()
}

func stopCandidateCWDs(cwd string) ([]string, error) {
	cwds := []string{cwd}
	packages, found, err := project.DiscoverWorkspacePackageDirs(cwd)
	if err != nil {
		return nil, err
	}
	if !found {
		return cwds, nil
	}
	for _, workspacePackage := range packages {
		cwds = append(cwds, workspacePackage.Dir)
	}
	return cwds, nil
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
	token, tokenPath, err := discoverWindowsTokenFunc(windowsUsersRoot)
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
		if err := startWindowsServiceFunc(ctx, tokenPath); err != nil {
			return routeManager{}, windowsRouterUnavailableError(err)
		}
		if err := waitForRouterHealth(ctx, windowsRouterHealthFunc, routerStartTimeout); err != nil {
			return routeManager{}, windowsRouterUnavailableError(err)
		}
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

func resolveStopTarget(routes []router.Route, target string) ([]router.Route, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, nil
	}
	for _, route := range routes {
		if route.Host == target {
			return []router.Route{route}, nil
		}
	}
	if strings.Contains(target, ".") && !strings.HasSuffix(target, ".localhost") {
		host := target + ".localhost"
		for _, route := range routes {
			if route.Host == host {
				return []router.Route{route}, nil
			}
		}
	}

	projectMatches := routesMatchingProjectName(routes, target)
	nameMatches := routesMatchingRouteName(routes, target)
	if len(projectMatches) > 0 && len(nameMatches) > 0 {
		if sameRouteHosts(projectMatches, nameMatches) {
			return projectMatches, nil
		}
		return nil, ambiguousProjectAndRouteError(target, projectMatches, nameMatches)
	}
	if len(projectMatches) > 0 {
		return projectMatches, nil
	}
	if len(nameMatches) == 1 {
		return nameMatches, nil
	}
	if len(nameMatches) > 1 {
		return nil, ambiguousRouteNameError(target, nameMatches)
	}
	return nil, nil
}

func routesMatchingProjectName(routes []router.Route, name string) []router.Route {
	var matches []router.Route
	for _, route := range routes {
		if route.ProjectName == name {
			matches = append(matches, route)
		}
	}
	return matches
}

func routesMatchingRouteName(routes []router.Route, name string) []router.Route {
	seen := map[string]bool{}
	var matches []router.Route
	for _, route := range routes {
		if route.Name != name && routeShortName(route) != name {
			continue
		}
		if seen[route.Host] {
			continue
		}
		seen[route.Host] = true
		matches = append(matches, route)
	}
	return matches
}

func routeShortName(route router.Route) string {
	host := strings.TrimSuffix(route.Host, ".localhost")
	before, _, _ := strings.Cut(host, ".")
	return before
}

func sameRouteHosts(a, b []router.Route) bool {
	if len(a) != len(b) {
		return false
	}
	hosts := make(map[string]bool, len(a))
	for _, route := range a {
		hosts[route.Host] = true
	}
	for _, route := range b {
		if !hosts[route.Host] {
			return false
		}
	}
	return true
}

func ambiguousProjectAndRouteError(target string, projectRoutes, routeMatches []router.Route) error {
	var out strings.Builder
	fmt.Fprintf(&out, "gohere error: %q matches a project and a route.\n\nProject:\n  %s\n", target, target)
	for _, route := range projectRoutes {
		fmt.Fprintf(&out, "    %s\n", route.Host)
	}
	fmt.Fprintln(&out, "\nRoute:")
	for _, route := range routeMatches {
		fmt.Fprintf(&out, "  %s\n", route.Host)
	}
	return errors.New(strings.TrimRight(out.String(), "\n"))
}

func ambiguousRouteNameError(target string, routes []router.Route) error {
	var out strings.Builder
	fmt.Fprintf(&out, "gohere error: route name %q is ambiguous.\n\nMatches:\n", target)
	for _, route := range routes {
		fmt.Fprintf(&out, "  %s\n", route.Host)
	}
	return errors.New(strings.TrimRight(out.String(), "\n"))
}

func pruneAdminRoutes(ctx context.Context, client adminClient) (int, error) {
	statuses, err := adminRouteStatuses(ctx, client)
	if err != nil {
		if errors.Is(err, admin.ErrUnauthorized) {
			return 0, staleRouterTokenError()
		}
		return 0, err
	}
	removed := 0
	for _, status := range statuses {
		if status.Status != lifecycle.RouteStatusDead {
			continue
		}
		if err := client.DeleteRoute(ctx, status.Route.Host); err != nil {
			if removed > 0 {
				return removed, fmt.Errorf("removed %d dead route%s before failing to delete %s: %w", removed, pluralS(removed), status.Route.Host, err)
			}
			return removed, fmt.Errorf("failed to delete %s: %w", status.Route.Host, err)
		}
		removed++
	}
	return removed, nil
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func adminRouteStatuses(ctx context.Context, client adminClient) ([]lifecycle.RouteStatus, error) {
	if statusClient, ok := client.(routeStatusClient); ok {
		statuses, err := statusClient.RouteStatuses(ctx)
		if err == nil {
			return convertAdminStatuses(statuses), nil
		}
		if errors.Is(err, admin.ErrUnauthorized) {
			return nil, err
		}
		if errors.Is(err, admin.ErrRouteStatusesUnsupported) {
			if probeClient, ok := client.(bridgeProbeClient); ok {
				return adminProbeRouteStatuses(ctx, client, probeClient)
			}
			return fallbackLocalRouteStatuses(ctx, client)
		}
		return nil, err
	}
	if probeClient, ok := client.(bridgeProbeClient); ok {
		return adminProbeRouteStatuses(ctx, client, probeClient)
	}
	routes, err := client.Routes(ctx)
	if err != nil {
		return nil, err
	}
	return lifecycle.RouteStatuses(routes), nil
}

func fallbackLocalRouteStatuses(ctx context.Context, client adminClient) ([]lifecycle.RouteStatus, error) {
	routes, err := client.Routes(ctx)
	if err != nil {
		return nil, err
	}
	statuses := make([]lifecycle.RouteStatus, 0, len(routes))
	for _, route := range routes {
		if routeOwnedByCurrentEnv(route) {
			statuses = append(statuses, lifecycle.RouteStatus{Route: route, Status: fallbackOwnedRouteStatus(route)})
			continue
		}
		statuses = append(statuses, lifecycle.RouteStatus{Route: route, Status: lifecycle.RouteStatusUnknown})
	}
	return statuses, nil
}

func routeOwnedByCurrentEnv(route router.Route) bool {
	owner := route.OwnerEnv
	if owner == "" && route.Source == "wsl" {
		owner = "wsl"
	}
	if owner != "" {
		return owner == runOwnerEnv()
	}
	return false
}

func fallbackOwnedRouteStatus(route router.Route) lifecycle.RouteStatusKind {
	if route.PID > 0 && !lifecycle.PIDAlive(route.PID) {
		return lifecycle.RouteStatusDead
	}
	return lifecycle.RouteStatusKind(probe.TargetStatus(route.Target))
}

func convertAdminStatuses(statuses []router.RouteStatus) []lifecycle.RouteStatus {
	converted := make([]lifecycle.RouteStatus, 0, len(statuses))
	for _, status := range statuses {
		routeStatus := lifecycle.RouteStatusKind(status.Status)
		if routeStatus == lifecycle.RouteStatusUnknown && routeOwnedByCurrentEnv(status.Route) {
			routeStatus = fallbackOwnedRouteStatus(status.Route)
		}
		converted = append(converted, lifecycle.RouteStatus{
			Route:  status.Route,
			Status: routeStatus,
		})
	}
	return converted
}

func adminProbeRouteStatuses(ctx context.Context, client adminClient, probeClient bridgeProbeClient) ([]lifecycle.RouteStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	routes, err := client.Routes(ctx)
	if err != nil {
		return nil, err
	}
	statuses := make([]lifecycle.RouteStatus, 0, len(routes))
	for _, route := range routes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		reachable, err := probeClient.ProbeTarget(ctx, route.Target)
		status := lifecycle.RouteStatusUnknown
		if err != nil {
			if errors.Is(err, admin.ErrUnauthorized) {
				return nil, err
			}
		} else if reachable {
			status = lifecycle.RouteStatusReady
		} else if routeOwnedByCurrentEnv(route) {
			status = fallbackOwnedRouteStatus(route)
		}
		statuses = append(statuses, lifecycle.RouteStatus{Route: route, Status: status})
	}
	return statuses, nil
}

func stopAdminRoutes(ctx context.Context, client adminClient, routes []router.Route) (lifecycle.StopResult, error) {
	var result lifecycle.StopResult
	for _, route := range routes {
		result.MatchedHost = route.Host
		action, reason := stopAction(route)
		if reason != "" {
			result.Skipped = append(result.Skipped, lifecycle.StopSkip{Host: route.Host, Reason: reason})
			continue
		}
		if action == stopActionTerminate {
			lifecycle.StopPID(route.PID)
			result.Stopped = true
		}
		if err := deleteAdminRoute(ctx, client, route.Host); err != nil {
			return result, err
		}
		result.Hosts = append(result.Hosts, route.Host)
	}
	return result, nil
}

func deleteAdminRoute(ctx context.Context, client adminClient, host string) error {
	if err := client.DeleteRoute(ctx, host); err != nil {
		return fmt.Errorf("could not delete route %s; route may still appear in gohere list: %w", host, err)
	}
	return nil
}

func stopStoreRoutes(store router.Store, selected []router.Route) (lifecycle.StopResult, error) {
	routes, err := store.Load()
	if err != nil {
		return lifecycle.StopResult{}, err
	}
	selectedHosts := make(map[string]bool, len(selected))
	for _, route := range selected {
		selectedHosts[route.Host] = true
	}
	var result lifecycle.StopResult
	kept := routes[:0]
	for _, route := range routes {
		if !selectedHosts[route.Host] {
			kept = append(kept, route)
			continue
		}
		result.MatchedHost = route.Host
		action, reason := stopAction(route)
		if reason != "" {
			result.Skipped = append(result.Skipped, lifecycle.StopSkip{Host: route.Host, Reason: reason})
			kept = append(kept, route)
			continue
		}
		if action == stopActionTerminate {
			lifecycle.StopPID(route.PID)
			result.Stopped = true
		}
		result.Hosts = append(result.Hosts, route.Host)
	}
	if err := store.Save(kept); err != nil {
		return result, err
	}
	return result, nil
}

type routeStopAction string

const (
	stopActionDelete    routeStopAction = "delete"
	stopActionTerminate routeStopAction = "terminate"
)

func stopAction(route router.Route) (routeStopAction, string) {
	if route.OwnerEnv != "" && route.OwnerEnv != runOwnerEnv() {
		return "", "route belongs to another environment."
	}
	if !lifecycle.PIDAlive(route.PID) || lifecycle.RouteStatuses([]router.Route{route})[0].Status == lifecycle.RouteStatusDead {
		return stopActionDelete, ""
	}
	if lifecycle.RouteProcessVerified(route) {
		return stopActionTerminate, ""
	}
	return "", "could not verify the original gohere process."
}

func stopAdminCurrent(ctx context.Context, client adminClient, cwd string) (string, bool, string, error) {
	result, err := stopAdminCWDs(ctx, client, []string{cwd})
	return result.MatchedHost, result.Stopped, result.Warning, err
}

func stopAdminCWDs(ctx context.Context, client adminClient, cwds []string) (lifecycle.StopResult, error) {
	routes, err := client.Routes(ctx)
	if err != nil {
		if errors.Is(err, admin.ErrUnauthorized) {
			return lifecycle.StopResult{}, staleRouterTokenError()
		}
		return lifecycle.StopResult{}, err
	}
	absCWDs, err := lifecycle.AbsCWDSet(cwds)
	if err != nil {
		return lifecycle.StopResult{}, err
	}
	var result lifecycle.StopResult
	for _, route := range routes {
		if !lifecycle.RouteMatchesAnyCWD(route, absCWDs) {
			continue
		}
		if route.OwnerEnv != "" && route.OwnerEnv != runOwnerEnv() {
			continue
		}
		result.MatchedHost = route.Host
		if !lifecycle.PIDAlive(route.PID) || lifecycle.RouteStatuses([]router.Route{route})[0].Status == lifecycle.RouteStatusDead {
			if err := deleteAdminRoute(ctx, client, route.Host); err != nil {
				return result, err
			}
			result.Hosts = append(result.Hosts, route.Host)
			continue
		}
		if lifecycle.RouteProcessVerified(route) {
			lifecycle.StopPID(route.PID)
			if err := deleteAdminRoute(ctx, client, route.Host); err != nil {
				return result, err
			}
			result.Hosts = append(result.Hosts, route.Host)
			result.Stopped = true
			continue
		}
		if result.Warning == "" {
			result.Warning = lifecycle.UnverifiedProcessWarning(route.PID)
		}
	}
	return result, nil
}

func printStopResult(stdout io.Writer, host string, stopped bool, warning string) {
	printStopResults(stdout, lifecycle.StopResult{
		Hosts:       stopResultHosts(host, warning),
		MatchedHost: host,
		Stopped:     stopped,
		Warning:     warning,
	})
}

func stopResultHosts(host, warning string) []string {
	if host == "" || warning != "" {
		return nil
	}
	return []string{host}
}

func printStopResults(stdout io.Writer, result lifecycle.StopResult) {
	if result.Warning != "" {
		fmt.Fprintln(stdout, result.Warning)
	}
	for _, skipped := range result.Skipped {
		fmt.Fprintf(stdout, "Skipped %s: %s\n", skipped.Host, skipped.Reason)
	}
	if len(result.Hosts) == 0 {
		if result.Warning != "" || len(result.Skipped) > 0 {
			return
		}
		fmt.Fprintln(stdout, "No running gohere project found for this folder.")
		return
	}
	for _, host := range result.Hosts {
		fmt.Fprintf(stdout, "Stopped %s.\n", host)
	}
}

func Doctor(ctx context.Context, stdout io.Writer) error {
	stateDir := router.DefaultStateDir()
	client, err := defaultAdminClientFunc()
	if err != nil {
		client = nil
	}
	return DoctorWithStore(ctx, stdout, stateDir, defaultStore(), client)
}

func DoctorWithStore(ctx context.Context, stdout io.Writer, stateDir string, store router.Store, client adminClient) error {
	return DoctorWithChecks(ctx, stdout, stateDir, store, client, DoctorChecks{
		Port80Status:        port80Status,
		LocalhostHTTPStatus: localhostHTTPStatusFunc,
	})
}

type Port80Status struct {
	OK     bool
	Detail string
	Hint   string
}

type LocalhostHTTPStatus struct {
	OK     bool
	Detail string
	Hint   string
}

type DoctorChecks struct {
	Port80Available      func() bool
	Port80Status         func() Port80Status
	LocalhostHTTPStatus  func(context.Context) LocalhostHTTPStatus
	SetcapEnabled        func(string) bool
	SystemdUserServiceOK func() (bool, bool)
	GOOS                 string
}

func DoctorWithChecks(ctx context.Context, stdout io.Writer, stateDir string, store router.Store, client adminClient, extra DoctorChecks) error {
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
		{Name: "stable binary", OK: exists(binaryPath), Detail: binaryPath, Hint: "Try: run gohere once to reinstall the local service binary."},
		{Name: "token", OK: exists(tokenPath), Detail: tokenPath, Hint: "Try: run gohere uninstall, then run gohere again."},
	}
	if info, err := os.Stat(tokenPath); goos != "windows" && err == nil {
		checks = append(checks, lifecycle.DoctorCheck{Name: "token permissions", OK: info.Mode().Perm() == 0600, Detail: info.Mode().Perm().String(), Hint: "Try: chmod 600 ~/.gohere/token"})
	}
	adminHealthy := false
	if client != nil {
		adminHealthy = client.Health(ctx) == nil
		checks = append(checks, lifecycle.DoctorCheck{Name: "admin API health", OK: adminHealthy, Hint: "Try: gohere uninstall, then run gohere again."})
	}
	if pid, err := os.ReadFile(pidPath); err == nil {
		checks = append(checks, lifecycle.DoctorCheck{Name: "service pid", OK: true, Detail: strings.TrimSpace(string(pid))})
	} else {
		checks = append(checks, lifecycle.DoctorCheck{Name: "service pid", OK: false, Detail: pidPath, Hint: "Try: run gohere once to start the service."})
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
			detail = "used by gohere service"
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
			detail = "used by gohere service"
		}
		checks = append(checks, lifecycle.DoctorCheck{Name: "port 80", OK: ok, Detail: detail, Hint: "Try: stop the process using port 80, then run gohere again."})
	}
	if extra.LocalhostHTTPStatus != nil {
		status := extra.LocalhostHTTPStatus(ctx)
		checks = append(checks, lifecycle.DoctorCheck{Name: ".localhost routing", OK: status.OK, Detail: status.Detail, Hint: status.Hint})
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
			checks = append(checks, lifecycle.DoctorCheck{Name: "systemd user service", OK: ok, Detail: detail, Hint: "Try: gohere service stop, then run gohere again."})
		}
	}
	checks = append(checks, bridgeDoctorChecks(ctx)...)
	fmt.Fprint(stdout, lifecycle.FormatDoctor(checks))
	return nil
}

func bridgeDoctorChecks(ctx context.Context) []lifecycle.DoctorCheck {
	if !detectWSLFunc() {
		return nil
	}
	checks := []lifecycle.DoctorCheck{{Name: "environment", OK: true, Detail: "WSL"}}
	if !windowsStableBinaryExists(windowsUsersRoot) {
		return append(checks, lifecycle.DoctorCheck{
			Name:   "windows service install",
			OK:     false,
			Detail: "missing",
			Hint:   "Run gohere from Windows first so WSL can use the Windows service.",
		})
	}
	if err := windowsRouterHealthFunc(ctx); err != nil {
		return append(checks, lifecycle.DoctorCheck{
			Name:   "windows service health",
			OK:     false,
			Detail: "unavailable",
			Hint:   "Run gohere from Windows first so WSL can use the Windows service.",
		})
	}
	token, _, err := discoverWindowsTokenFunc(windowsUsersRoot)
	if err != nil {
		detail := "unavailable"
		if errors.Is(err, bridge.ErrWindowsTokenNotFound) {
			detail = "missing"
		}
		return append(checks, lifecycle.DoctorCheck{
			Name:   "windows service token",
			OK:     false,
			Detail: detail,
			Hint:   "Run gohere from Windows first so WSL can use the Windows service.",
		})
	}
	client := newWindowsAdminClientFunc(token)
	if _, err := client.Routes(ctx); err != nil {
		if errors.Is(err, admin.ErrUnauthorized) {
			return append(checks, lifecycle.DoctorCheck{Name: "windows service", OK: false, Detail: "auth failed", Hint: "Try: gohere service stop in the side where the old service is running."})
		}
		return append(checks, lifecycle.DoctorCheck{Name: "windows service", OK: false, Detail: "unavailable: " + err.Error(), Hint: "Run gohere doctor from Windows for more details."})
	}
	return append(checks, lifecycle.DoctorCheck{Name: "windows service", OK: true, Detail: "available"})
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
	return router.NewRouteStore(filepath.Join(router.DefaultStateDir(), router.RoutesFilename))
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

func localhostHTTPStatus(ctx context.Context) LocalhostHTTPStatus {
	return localhostHTTPStatusForURL(ctx, doctorLocalhostProbeURL)
}

func localhostHTTPStatusForURL(ctx context.Context, probeURL string) LocalhostHTTPStatus {
	ctx, cancel := context.WithTimeout(ctx, doctorLocalhostProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		return LocalhostHTTPStatus{
			OK:     false,
			Detail: "invalid probe URL",
			Hint:   err.Error(),
		}
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{
		Timeout:   doctorLocalhostProbeTimeout,
		Transport: transport,
	}
	resp, err := client.Do(req)
	if err != nil {
		return LocalhostHTTPStatus{
			OK:     false,
			Detail: "unreachable: " + compactDoctorDetail(err.Error()),
			Hint:   "Try: gohere setup. In Windows/WSL setups, run gohere doctor from the OS where the browser runs too.",
		}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return LocalhostHTTPStatus{
			OK:     false,
			Detail: "read failed",
			Hint:   err.Error(),
		}
	}
	text := string(body)
	if strings.Contains(text, "gohere route missing") || strings.Contains(text, "No gohere route is running") {
		return LocalhostHTTPStatus{OK: true, Detail: "reached gohere router"}
	}
	return LocalhostHTTPStatus{
		OK:     false,
		Detail: "unexpected response: " + resp.Status,
		Hint:   "Another process may own port 80, or .localhost may resolve outside gohere. In Windows/WSL setups, run gohere doctor from the OS where the browser runs too.",
	}
}

func compactDoctorDetail(detail string) string {
	const max = 140
	if len(detail) <= max {
		return detail
	}
	return detail[:max-3] + "..."
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
