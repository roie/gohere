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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/roie/gohere/internal/admin"
	"github.com/roie/gohere/internal/bridge"
	localcert "github.com/roie/gohere/internal/cert"
	"github.com/roie/gohere/internal/certtrust"
	"github.com/roie/gohere/internal/cli"
	"github.com/roie/gohere/internal/companion"
	appconfig "github.com/roie/gohere/internal/config"
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
	promptInput                                          = io.Reader(os.Stdin)
	setupFunc                                            = setupCurrentEnvironment
	setupLinuxFunc                                       = setup.Linux
	setupWindowsFunc                                     = setup.Windows
	setupDarwinFunc                                      = setup.Darwin
	startInstalledRouterFunc                             = startInstalledRouter
	defaultAdminClientFunc   func() (adminClient, error) = func() (adminClient, error) {
		return defaultAdminClient()
	}
	startRunnerFunc             = runner.Start
	detectWSLFunc               = bridge.DetectWSL
	detectWSL2Func              = bridge.DetectWSL2
	routerHealthFunc            = func(ctx context.Context) error { return admin.NewClient("http://127.0.0.1:39399", "").Health(ctx) }
	serviceStopFunc             = func(ctx context.Context) error { return ServiceStopWithConfig(ctx, io.Discard, ServiceStopConfig{}) }
	execCommandContext          = exec.CommandContext
	windowsHTTPSCATrustedFunc   = windowsHTTPSCATrusted
	repairWindowsHTTPSTrustFunc = repairWindowsHTTPSTrust
	currentWSLIPFunc            = bridge.CurrentWSLIP
	probeBridgeFunc             = func(ctx context.Context, client bridgeProbeClient, wslIP string) (bool, string, error) {
		return bridge.ProbeBridge(ctx, client, wslIP)
	}
	localhostHTTPStatusFunc = localhostHTTPStatus
	openBrowserFunc         = func(ctx context.Context, url string) error {
		return opener.Open(ctx, runtime.GOOS, detectWSLFunc(), url)
	}
	chooseFreePortForHostFunc = runner.ChooseFreePortForHost
)

const routerStartTimeout = 10 * time.Second
const AutoURLScheme = "auto"

var (
	routeLeaseDuration = 90 * time.Second
	routeLeaseInterval = 30 * time.Second
)

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

type wslOwnerClient interface {
	adminClient
	routeStatusClient
}

type bridgeProbeClient interface {
	ProbeTarget(context.Context, string) (bool, error)
}

type bridgeAdminClient interface {
	adminClient
	bridgeProbeClient
}

type runRouter struct {
	Client          adminClient
	RouteTargetHost string
	ChildHost       string
	RouteSource     string
	OwnerEnv        string
	OwnerInstance   string
	Distro          string
	LinuxUser       string
	RunnerID        string
	PublicTransport wslPublicTransport
	RouterLabel     string
	PublicURLScheme string
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
	OwnerInstance       string
	Distro              string
	LinuxUser           string
	RunnerID            string
	ListenHost          string
	RouterLabel         string
	StaticBindHost      string
	ManagedPort         bool
	FixedPort           bool
	Mode                string
	URLScheme           string
	AuthorityURLScheme  string
}

var (
	wslTargetProbeTimeout  = 5 * time.Second
	wslTargetProbeInterval = 100 * time.Millisecond
)

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
	if cmd.TargetPort == 0 {
		if explicitPort, ok := runner.ExplicitPort(scriptCommand); ok {
			port = explicitPort
			env = runner.ChildEnvForHost(os.Environ(), port, childHost)
		}
	}
	fixedPort := cmd.TargetPort != 0
	if _, ok := runner.ExplicitPort(scriptCommand); ok {
		fixedPort = true
	}

	pm, _, err := project.DetectPackageManager(projectDir(packagePath))
	if err != nil {
		return RunPlan{}, err
	}
	injected := runner.InjectPortArgsForHost(scriptCommand, port, cmd.PortFlag, childHost)
	command := runner.BuildScriptCommand(pm, cmd.Script, injected)
	managedPort := injectedArgsControlPort(injected, cmd.PortFlag)
	host, err := project.HostnameForProject(cwd)
	if err != nil {
		return RunPlan{}, err
	}
	projectRoot := projectDir(packagePath)
	projectName, err := project.ProjectNameForRoot(projectRoot)
	if err != nil {
		return RunPlan{}, err
	}
	return applyRunOptions(cmd, RunPlan{Command: command, Env: env, Port: port, Host: host, Name: strings.TrimSuffix(host, ".localhost"), CWD: cwd, ProjectRoot: projectRoot, ProjectName: projectName, ManagedPort: managedPort, FixedPort: fixedPort})
}

func applyRunOptions(cmd cli.Command, plan RunPlan) (RunPlan, error) {
	plan.FixedPort = plan.FixedPort || cmd.TargetPort != 0
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
		rr, err := resolveRunRouter(ctx, stderr, cmd)
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

	if classifyRunIntent(cmd, plan) == runIntentPlanned {
		if err := ensureRunRouter(); err != nil {
			return err
		}
		if detectWSLFunc() && shouldPrepareForChildBindHost(cmd, resolvedRouter.ChildHost) {
			plan, err = prepareRunWithHost(cmd, cwd, resolvedRouter.ChildHost)
			if err != nil {
				return err
			}
			applyRunRouter(&plan, resolvedRouter)
		}
		applyPublicURLScheme(&plan, cmd)
		return runPlannedSingle(ctx, cmd, plan, adminClient, stdout, stderr)
	}

	liveStdout := newReplayWriter(32*1024, stdout)
	liveStderr := newReplayWriter(32*1024, stderr)

	result, err := startRunnerFunc(ctx, runner.Config{
		Command:             plan.Command,
		Dir:                 runnerDirForRun(cmd, plan),
		Env:                 plan.Env,
		ChosenPort:          plan.Port,
		RequireDetectedPort: plan.RequireDetectedPort,
		Stdout:              liveStdout,
		Stderr:              liveStderr,
		StartupTimeout:      15 * time.Second,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		replayCapturedOutput(stderr, liveStdout.capture(), liveStderr.capture())
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
	applyPublicURLScheme(&plan, cmd)

	cleanup, err := registerRoute(ctx, adminClient, cmd, plan, result.Port, result.PID(), stdout, stderr)
	if err != nil {
		return err
	}
	defer cleanup()
	startLiveOutput(liveStdout, liveStderr, cmd.Verbose)
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
		return false, nil
	}

	pm, _, err := project.DetectPackageManager(cwd)
	if err != nil {
		return true, err
	}

	return true, runWorkspace(ctx, cmd, cwd, pm, packages, stdout, stderr)
}

func runWorkspace(ctx context.Context, cmd cli.Command, root string, pm project.PackageManager, packages []project.WorkspacePackage, stdout, stderr io.Writer) error {
	var items []multiRunItem
	var adminClient adminClient
	routerResolved := false
	ensureRunRouter := func() (runRouter, error) {
		if routerResolved {
			return runRouter{Client: adminClient}, nil
		}
		rr, err := resolveRunRouter(ctx, stderr, cmd)
		if err != nil {
			return runRouter{}, err
		}
		adminClient = rr.Client
		routerResolved = true
		return rr, nil
	}

	var resolvedRouter runRouter
	prepareHost := "127.0.0.1"
	isWSL := detectWSLFunc()
	if isWSL {
		rr, err := ensureRunRouter()
		if err != nil {
			return err
		}
		resolvedRouter = rr
		if rr.ChildHost != "" {
			prepareHost = rr.ChildHost
		}
	}

	sources := make([]string, len(packages))
	for i, workspacePackage := range packages {
		sources[i] = workspacePackage.ShortName
	}
	plans, err := prepareDistinctGroupPlans(len(packages), func(i int) (RunPlan, error) {
		workspacePackage := packages[i]
		plan, err := prepareWorkspacePackageRun(cmd, root, pm, workspacePackage, prepareHost)
		if err != nil {
			return RunPlan{}, err
		}
		if isWSL {
			applyRunRouter(&plan, resolvedRouter)
		}
		return plan, nil
	}, func(i int) string {
		return sources[i]
	})
	if err != nil {
		return err
	}
	for i, workspacePackage := range packages {
		itemCmd := cmd
		itemCmd.Script = workspacePackage.ShortName
		itemCmd.Scripts = nil
		itemCmd.ExplicitScript = true
		items = append(items, multiRunItem{cmd: itemCmd, plan: plans[i]})
	}
	if !routerResolved {
		rr, err := ensureRunRouter()
		if err != nil {
			return err
		}
		resolvedRouter = rr
	}
	for i := range items {
		applyRunRouter(&items[i].plan, resolvedRouter)
		applyPublicURLScheme(&items[i].plan, items[i].cmd)
	}
	return runPlannedGroup(ctx, cmd.Verbose, items, adminClient, stdout, stderr)
}

func injectedArgsControlPort(args []string, portFlag string) bool {
	for _, arg := range args {
		switch arg {
		case "--port", "-p":
			return true
		}
		if strings.HasPrefix(arg, "--port=") || (strings.HasPrefix(arg, "-p") && len(arg) > len("-p")) {
			return true
		}
		if portFlag != "" && arg == portFlag {
			return true
		}
		if portFlag != "" && strings.HasPrefix(arg, portFlag+"=") {
			return true
		}
	}
	return false
}

func prepareWorkspacePackageRun(cmd cli.Command, root string, pm project.PackageManager, workspacePackage project.WorkspacePackage, childHost string) (RunPlan, error) {
	port, err := chooseFreePortForHostFunc(childHost)
	if err != nil {
		return RunPlan{}, err
	}
	env := runner.ChildEnvForHost(os.Environ(), port, childHost)
	fixedPort := cmd.TargetPort != 0
	if explicitPort, ok := runner.ExplicitPort(workspacePackage.Script); ok {
		port = explicitPort
		fixedPort = true
		env = runner.ChildEnvForHost(os.Environ(), port, childHost)
	}
	injected := runner.InjectPortArgsForHost(workspacePackage.Script, port, cmd.PortFlag, childHost)
	command := runner.BuildScriptCommand(pm, cmd.Script, injected)
	managedPort := injectedArgsControlPort(injected, cmd.PortFlag)
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
		FixedPort:   fixedPort,
		Mode:        "workspace",
	}, nil
}

const maxDistinctGroupPortAttempts = 8

func prepareDistinctGroupPlans(count int, prepare func(int) (RunPlan, error), source func(int) string) ([]RunPlan, error) {
	plans := make([]RunPlan, count)
	fixedPorts := map[int]bool{}
	for i := range count {
		plan, err := prepare(i)
		if err != nil {
			return nil, err
		}
		plans[i] = plan
		if plan.FixedPort {
			fixedPorts[plan.Port] = true
		}
	}

	acceptedAutoPorts := map[int]bool{}
	for i := range count {
		if plans[i].FixedPort {
			continue
		}
		attempts := 1
		for fixedPorts[plans[i].Port] || acceptedAutoPorts[plans[i].Port] {
			if attempts >= maxDistinctGroupPortAttempts {
				return nil, fmt.Errorf("gohere error: could not choose a distinct auto port for %s after %d attempts", source(i), maxDistinctGroupPortAttempts)
			}
			plan, err := prepare(i)
			if err != nil {
				return nil, err
			}
			plans[i] = plan
			attempts++
		}
		acceptedAutoPorts[plans[i].Port] = true
	}
	return plans, nil
}

type multiRunItem struct {
	cmd    cli.Command
	plan   RunPlan
	result *runner.Result
	reused bool
}

func runMulti(ctx context.Context, cmd cli.Command, cwd string, stdout, stderr io.Writer) error {
	var items []multiRunItem
	var adminClient adminClient
	routerResolved := false
	ensureRunRouter := func() (runRouter, error) {
		if routerResolved {
			return runRouter{Client: adminClient}, nil
		}
		rr, err := resolveRunRouter(ctx, stderr, cmd)
		if err != nil {
			return runRouter{}, err
		}
		adminClient = rr.Client
		routerResolved = true
		return rr, nil
	}

	var resolvedRouter runRouter
	prepareHost := "127.0.0.1"
	isWSL := detectWSLFunc()
	if isWSL {
		rr, err := ensureRunRouter()
		if err != nil {
			return err
		}
		resolvedRouter = rr
		if rr.ChildHost != "" {
			prepareHost = rr.ChildHost
		}
	}

	plans, err := prepareDistinctGroupPlans(len(cmd.Scripts), func(i int) (RunPlan, error) {
		script := cmd.Scripts[i]
		itemCmd := cmd
		itemCmd.Script = script
		itemCmd.Scripts = nil
		plan, err := prepareRunWithHost(itemCmd, cwd, prepareHost)
		if err != nil {
			return RunPlan{}, err
		}
		if plan.Static {
			return RunPlan{}, errors.New("gohere error: multiple projects only support package scripts")
		}
		plan.Host = multiScriptHost(script, plan.Host)
		plan.Name = strings.TrimSuffix(plan.Host, ".localhost")
		plan.Mode = "multi"
		if isWSL {
			applyRunRouter(&plan, resolvedRouter)
		}
		return plan, nil
	}, func(i int) string {
		return cmd.Scripts[i]
	})
	if err != nil {
		return err
	}
	for i, script := range cmd.Scripts {
		itemCmd := cmd
		itemCmd.Script = script
		itemCmd.Scripts = nil
		items = append(items, multiRunItem{cmd: itemCmd, plan: plans[i]})
	}
	if !routerResolved {
		rr, err := ensureRunRouter()
		if err != nil {
			return err
		}
		resolvedRouter = rr
	}
	for i := range items {
		applyRunRouter(&items[i].plan, resolvedRouter)
		applyPublicURLScheme(&items[i].plan, items[i].cmd)
	}
	return runPlannedGroup(ctx, cmd.Verbose, items, adminClient, stdout, stderr)
}

func multiScriptHost(script, baseHost string) string {
	base := strings.TrimSuffix(baseHost, ".localhost")
	label := script
	if index := strings.LastIndex(script, ":"); index >= 0 && index < len(script)-1 {
		label = script[index+1:]
	}
	return project.NormalizeHostnameName(label) + "." + base + ".localhost"
}

func reusableExistingRoute(plan RunPlan, statuses []lifecycle.RouteStatus) (router.Route, bool) {
	absCWDs, err := lifecycle.AbsCWDSet([]string{plan.CWD})
	if err != nil {
		return router.Route{}, false
	}
	for _, status := range statuses {
		if status.Status != lifecycle.RouteStatusReady && status.Status != lifecycle.RouteStatusUnknown {
			continue
		}
		if !strings.EqualFold(status.Route.Host, plan.Host) {
			continue
		}
		if status.Route.PreferredScheme != plan.URLScheme {
			continue
		}
		if status.Route.OwnerEnv == "wsl" &&
			(status.Route.LeaseExpiresAt.IsZero() || router.RouteLeaseExpired(status.Route, time.Now())) {
			continue
		}
		if lifecycle.RouteMatchesAnyCWD(status.Route, absCWDs) {
			return status.Route, true
		}
	}
	return router.Route{}, false
}

func serviceDiscoveryLabel(item multiRunItem) string {
	name := runName(item.cmd)
	if index := strings.LastIndex(name, ":"); index >= 0 && index < len(name)-1 {
		name = name[index+1:]
	}
	if name != "" {
		return project.NormalizeHostnameName(name)
	}
	host := strings.TrimSuffix(item.plan.Host, ".localhost")
	if label, _, ok := strings.Cut(host, "."); ok {
		return label
	}
	return host
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
	running := 0
	for _, item := range items {
		if item.result != nil {
			running++
		}
	}
	if running == 0 {
		return nil
	}
	done := make(chan waitResult, running)
	for _, item := range items {
		if item.result == nil {
			continue
		}
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
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		for range running {
			select {
			case <-done:
			case <-timer.C:
				return nil
			}
		}
		return nil
	}
}

func registerRoute(ctx context.Context, adminClient adminClient, cmd cli.Command, plan RunPlan, port, pid int, stdout, stderr io.Writer) (func(), error) {
	if lifecycleClient, ok := adminClient.(routeLifecycleClient); ok {
		return registerDetectedRouteLifecycle(ctx, adminClient, lifecycleClient, cmd, plan, port, pid, stdout, stderr)
	}
	return registerLegacyRoute(ctx, adminClient, cmd, plan, port, pid, stdout, stderr)
}

func registerLegacyRoute(ctx context.Context, adminClient adminClient, cmd cli.Command, plan RunPlan, port, pid int, stdout, stderr io.Writer) (func(), error) {
	if cmd.ShareMode != "" {
		return nil, errors.New("router control does not support LAN sharing; update gohere")
	}
	routes, err := adminClient.Routes(ctx)
	if err != nil {
		if errors.Is(err, admin.ErrUnauthorized) {
			return nil, staleRouterTokenError()
		}
		return nil, err
	}
	plan.Host = resolveRouteHost(plan, toRegisteredRoutes(routes))
	plan.Name = strings.TrimSuffix(plan.Host, ".localhost")
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
		PreferredScheme: plan.URLScheme,
		URLPath:         plan.URLPath,
		ProjectRoot:     plan.ProjectRoot,
		ProjectName:     plan.ProjectName,
		Source:          plan.RouteSource,
		OwnerCWD:        plan.CWD,
		OwnerEnv:        ownerEnv,
		OwnerInstance:   plan.OwnerInstance,
		Distro:          plan.Distro,
		LinuxUser:       plan.LinuxUser,
		RunnerID:        plan.RunnerID,
		ListenTarget:    routeTarget(plan.ListenHost, port),
		StartedAt:       time.Now().UTC(),
		ProcessIdentity: processIdentity,
	}
	if ownerEnv == "wsl" && route.RunnerID != "" {
		route.LeaseExpiresAt = time.Now().UTC().Add(routeLeaseDuration)
	}
	if ownerEnv == "wsl" {
		probeClient, ok := adminClient.(bridgeProbeClient)
		if !ok {
			return nil, errors.New("Windows authority does not support WSL target probes")
		}
		reachable, err := waitForWSLTarget(ctx, probeClient, route.Target)
		if err != nil {
			return nil, fmt.Errorf("Windows could not probe WSL target %s: %w", route.Target, err)
		}
		if !reachable {
			return nil, fmt.Errorf("Windows cannot reach WSL target %s; the dev server may not support the selected bind address", route.Target)
		}
	}
	if err := adminClient.UpsertRoute(ctx, route); err != nil {
		if errors.Is(err, admin.ErrUnauthorized) {
			return nil, staleRouterTokenError()
		}
		return nil, err
	}
	stopLease := startRouteLease(ctx, adminClient, route, stderr)

	publicURL := publicRouteURLForScheme(plan.URLScheme, route.Host, plan.URLPath)
	fmt.Fprint(stdout, runSuccessOutputForScheme(cmd, plan.URLScheme, route.Host, plan.URLPath))
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
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			stopLease()
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := deleteOwnedRouteRegistration(cleanupCtx, adminClient, route); err != nil && stderr != nil {
				if routeCleanupTimedOut(err) && !routeStillRegistered(adminClient, route.Host) {
					return
				}
				fmt.Fprintln(stderr, routeCleanupWarning(route.Host, err))
			}
		})
	}
	stopContextCleanup := context.AfterFunc(ctx, cleanup)
	return func() {
		stopContextCleanup()
		cleanup()
	}, nil
}

func waitForWSLTarget(ctx context.Context, client bridgeProbeClient, target string) (bool, error) {
	deadline := time.Now().Add(wslTargetProbeTimeout)
	for {
		reachable, err := client.ProbeTarget(ctx, target)
		if err != nil || reachable {
			return reachable, err
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		timer := time.NewTimer(wslTargetProbeInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, ctx.Err()
		case <-timer.C:
		}
	}
}

func deleteOwnedRouteRegistration(ctx context.Context, client adminClient, expected router.Route) error {
	if expected.ID != "" && expected.Generation != 0 {
		if lifecycleClient, ok := client.(routeLifecycleClient); ok {
			return lifecycleClient.DeleteRouteRef(ctx, expected.Ref())
		}
	}
	if expected.RunnerID != "" {
		routes, err := client.Routes(ctx)
		if err != nil {
			return err
		}
		found := false
		for _, current := range routes {
			if !strings.EqualFold(current.Host, expected.Host) {
				continue
			}
			found = true
			if current.OwnerInstance != expected.OwnerInstance || current.RunnerID != expected.RunnerID {
				return nil
			}
			break
		}
		if !found {
			return nil
		}
	}
	return client.DeleteRoute(ctx, expected.Host)
}

func startRouteLease(ctx context.Context, client adminClient, route router.Route, stderr io.Writer) func() {
	if route.OwnerEnv != "wsl" || route.RunnerID == "" {
		return func() {}
	}
	leaseCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(routeLeaseInterval)
		defer ticker.Stop()
		warned := false
		for {
			select {
			case <-leaseCtx.Done():
				return
			case <-ticker.C:
				route.LeaseExpiresAt = time.Now().UTC().Add(routeLeaseDuration)
				renewCtx, renewCancel := context.WithTimeout(leaseCtx, 5*time.Second)
				err := client.UpsertRoute(renewCtx, route)
				renewCancel()
				if err != nil && !warned && stderr != nil {
					fmt.Fprintf(stderr, "gohere could not renew the Windows route lease for %s: %v\n", route.Host, err)
					warned = true
				} else if err == nil {
					warned = false
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}

func routeCleanupWarning(host string, err error) string {
	if routeCleanupTimedOut(err) {
		return fmt.Sprintf("Could not remove route %s before shutdown; run gohere prune if it still appears in gohere list.", host)
	}
	return fmt.Sprintf("Could not remove route %s: %v", host, err)
}

func routeCleanupTimedOut(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if os.IsTimeout(err) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func routeStillRegistered(client adminClient, host string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		routes, err := client.Routes(ctx)
		if err != nil {
			return true
		}
		found := false
		for _, route := range routes {
			if strings.EqualFold(route.Host, host) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
		select {
		case <-ctx.Done():
			return true
		case <-ticker.C:
		}
	}
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

func resolveRunRouter(ctx context.Context, stderr io.Writer, cmd cli.Command) (runRouter, error) {
	local := func() (runRouter, error) {
		client, err := defaultAdminClientFunc()
		health := routerHealthFunc
		if err == nil {
			health = client.Health
		} else {
			client = nil
		}
		if err := ensureRouter(ctx, stderr, health, requiresHTTPSSetup(cmd)); err != nil {
			return runRouter{}, err
		}
		if err := ensureHTTPSBrowserTrust(ctx, cmd); err != nil {
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

	requiredCapabilities := []string{
		"control.bootstrap",
		"control.ca-certificate",
		"control.delete-route",
		"control.probe-target",
		"control.routes",
		"control.route-statuses",
		"control.upsert-route",
		companion.CapabilityReserveRoutes,
		companion.CapabilityActivateRoutes,
		companion.CapabilityReleaseRoutes,
		companion.CapabilityRenewRoutes,
		companion.CapabilityDeleteRouteRef,
	}
	opened, err := openWindowsCompanion(ctx, requiredCapabilities...)
	if err != nil {
		return runRouter{}, err
	}
	if !opened.Info.RouterInstalled || strings.TrimSpace(opened.Info.CAFingerprint) == "" {
		if err := promptAndSetupWSLAuthority(ctx, stderr); err != nil {
			return runRouter{}, err
		}
	}
	resolved, err := resolveWindowsCompanion(ctx, requiredCapabilities...)
	if err != nil {
		return runRouter{}, err
	}
	identity, err := ensureWSLRunIdentityFunc(ctx, resolved.Info, stderr)
	if err != nil {
		return runRouter{}, err
	}
	publicTransport, err := ensureWSLPublicTransportFunc(ctx, resolved.Info, resolved.Executable)
	if err != nil {
		return runRouter{}, err
	}
	client := resolved.Control
	if err := ensureSingleActiveWSLOwner(ctx, client, identity); err != nil {
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
		OwnerInstance:   identity.OwnerInstance,
		Distro:          identity.Distro,
		LinuxUser:       identity.LinuxUser,
		RunnerID:        identity.RunnerID,
		PublicTransport: publicTransport,
		RouterLabel:     "Windows",
		PublicURLScheme: companionPublicURLScheme(resolved.Info),
		Bridge:          true,
	}, nil
}

func companionPublicURLScheme(info companion.Info) string {
	for _, listener := range info.Listeners {
		if strings.EqualFold(listener.Name, "https") {
			return "https"
		}
	}
	return "http"
}

func ensureSingleActiveWSLOwner(ctx context.Context, client wslOwnerClient, identity wslRunIdentity) error {
	statuses, err := client.RouteStatuses(ctx)
	if err != nil {
		return err
	}
	for _, status := range statuses {
		route := status.Route
		if route.OwnerEnv != "wsl" || route.OwnerInstance == "" || route.OwnerInstance == identity.OwnerInstance {
			continue
		}
		owner := route.OwnerInstance
		if route.Distro != "" || route.LinuxUser != "" {
			owner = strings.Trim(route.Distro+"/"+route.LinuxUser, "/")
		}
		if !router.RouteLeaseExpired(route, time.Now()) {
			return fmt.Errorf("another WSL owner (%s) is already using the Windows gohere authority; concurrent WSL distributions or Linux users are not supported in this release", owner)
		}
		if status.Status == "dead" {
			return fmt.Errorf("foreign WSL route %s from %s has an expired lease and a dead target; run gohere prune, then retry", route.Host, owner)
		}
		targetStatus := strings.TrimSpace(status.Status)
		if targetStatus == "" {
			targetStatus = "unknown"
		}
		return fmt.Errorf("foreign WSL route %s from %s has an expired lease but its target is %s; refusing automatic ownership transfer", route.Host, owner, targetStatus)
	}
	return nil
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
	if rr.OwnerInstance != "" {
		plan.OwnerInstance = rr.OwnerInstance
	}
	if rr.Distro != "" {
		plan.Distro = rr.Distro
	}
	if rr.LinuxUser != "" {
		plan.LinuxUser = rr.LinuxUser
	}
	if rr.RunnerID != "" {
		plan.RunnerID = rr.RunnerID
	}
	if rr.ChildHost != "" {
		plan.ListenHost = rr.ChildHost
	}
	if rr.RouterLabel != "" {
		plan.RouterLabel = rr.RouterLabel
	}
	if rr.PublicURLScheme != "" {
		plan.AuthorityURLScheme = rr.PublicURLScheme
	}
	if rr.ChildHost != "" && plan.Static {
		plan.StaticBindHost = rr.ChildHost
	}
	if rr.ChildHost != "" && !plan.Static {
		plan.Env = runner.ChildEnvForHost(plan.Env, plan.Port, rr.ChildHost)
		plan.Command = withHost(plan.Command, rr.ChildHost)
	}
	if rr.OwnerEnv == "wsl" {
		plan.Env = applyWSLTrustEnvironment(plan.Env, router.DefaultStateDir())
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

func commandOutputError(command string, output []byte, err error) error {
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		return fmt.Errorf("%s failed: %w", command, err)
	}
	return fmt.Errorf("%s failed: %w: %s", command, err, detail)
}

func windowsRouterCannotReachWSLError(cause error) error {
	msg := "The Windows gohere authority could not reach the WSL dev-server listener.\n\nGohere tried Windows-visible loopback and the current WSL2 address. The dev server may not support the required bind address.\n\nRun:\n  gohere doctor"
	if cause != nil {
		return fmt.Errorf("%s\n\nDetails: %w", msg, cause)
	}
	return errors.New(msg)
}

func staleRouterTokenError() error {
	return errors.New("The gohere router rejected its local control credential.\n\nRun from this shell:\n  gohere service stop\n  gohere setup")
}

func localRouterControlError(goos, stateDir string) error {
	if goos == "windows" {
		stableBinary := filepath.Join(stateDir, "bin", stableBinaryName(goos))
		tokenPath := filepath.Join(stateDir, "token")
		if !exists(stableBinary) || !exists(tokenPath) {
			return errors.New("A non-Windows process is occupying gohere's .localhost service ports.\n\nRun:\n  gohere doctor\n\nGohere will not start a second router or take over the existing process.")
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
	return runSuccessOutputForScheme(cmd, "http", host, urlPath)
}

func runSuccessOutputForScheme(cmd cli.Command, scheme, host, urlPath string) string {
	label := "gohere"
	if cmd.Kind == cli.CommandRun {
		switch {
		case cmd.TargetPath != "":
			label += " " + cmd.TargetPath
		case cmd.Script != "" && (cmd.Script != "dev" || urlPath != ""):
			label += " " + cmd.Script
		}
	}
	return fmt.Sprintf("%s \u2192 %s://%s%s\n", label, normalizedURLScheme(scheme), host, escapedURLPath(urlPath))
}

func publicRouteURLForScheme(scheme, host, urlPath string) string {
	return fmt.Sprintf("%s://%s%s", normalizedURLScheme(scheme), host, escapedURLPath(urlPath))
}

func applyPublicURLScheme(plan *RunPlan, cmd cli.Command) {
	if plan.URLScheme == "" {
		plan.URLScheme = publicURLScheme(cmd, plan.AuthorityURLScheme)
	}
}

func publicURLScheme(cmd cli.Command, authorityScheme string) string {
	if cmd.HTTP {
		return "http"
	}
	if cmd.URLScheme == "https" {
		return "https"
	}
	if cmd.URLScheme == AutoURLScheme {
		if authorityScheme != "" {
			return normalizedURLScheme(authorityScheme)
		}
		cfg, err := appconfig.Load(router.DefaultStateDir())
		if err == nil && cfg.HTTPS {
			return "https"
		}
	}
	return "http"
}

func normalizedURLScheme(scheme string) string {
	if scheme == "https" {
		return "https"
	}
	return "http"
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

type replayWriter struct {
	mu     sync.Mutex
	cap    *limitedCapture
	target io.Writer
	live   bool
}

func newReplayWriter(max int, target io.Writer) *replayWriter {
	return &replayWriter{cap: newLimitedCapture(max), target: target}
}

func startLiveOutput(stdoutWriter, stderrWriter *replayWriter, replayStartup bool) {
	stdoutWriter.Start(replayStartup)
	stderrWriter.Start(replayStartup)
}

func (w *replayWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.cap.Write(p)
	if w.live && w.target != nil {
		_, _ = w.target.Write(p)
	}
	w.mu.Unlock()
	return len(p), nil
}

func (w *replayWriter) Start(replay bool) {
	w.mu.Lock()
	if w.live {
		w.mu.Unlock()
		return
	}
	w.live = true
	target := w.target
	captured := ""
	if replay {
		captured = w.cap.String()
	}
	w.mu.Unlock()

	if target != nil && captured != "" {
		_, _ = io.WriteString(target, captured)
	}
}

func (w *replayWriter) capture() *limitedCapture {
	return w.cap
}

type linePrefixWriter struct {
	mu          sync.Mutex
	target      io.Writer
	prefix      string
	atLineStart bool
}

func newLinePrefixWriter(target io.Writer, prefix string) io.Writer {
	return &linePrefixWriter{target: target, prefix: prefix, atLineStart: true}
}

func (w *linePrefixWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.target == nil {
		return len(p), nil
	}
	written := len(p)
	for len(p) > 0 {
		if w.atLineStart {
			if _, err := io.WriteString(w.target, w.prefix); err != nil {
				return written, err
			}
			w.atLineStart = false
		}
		idx := bytes.IndexByte(p, '\n')
		if idx < 0 {
			if _, err := w.target.Write(p); err != nil {
				return written, err
			}
			return written, nil
		}
		if _, err := w.target.Write(p[:idx+1]); err != nil {
			return written, err
		}
		w.atLineStart = true
		p = p[idx+1:]
	}
	return written, nil
}

func ensureRouter(ctx context.Context, out io.Writer, health func(context.Context) error, requireHTTPSSetup bool) error {
	if err := health(ctx); err == nil && !requireHTTPSSetup {
		return nil
	} else if err == nil && requireHTTPSSetup {
		return promptAndRunSetup(ctx, out, health, true)
	}
	if err := startInstalledRouterFunc(ctx); err == nil {
		if err := waitForRouterHealth(ctx, health, routerStartTimeout); err == nil {
			if requireHTTPSSetup {
				return promptAndRunSetup(ctx, out, health, true)
			}
			return nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return installedRouterUnavailableError(err)
	}
	return promptAndRunSetup(ctx, out, health, false)
}

func promptAndRunSetup(ctx context.Context, out io.Writer, health func(context.Context) error, stopExisting bool) error {
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
	if stopExisting {
		if err := serviceStopFunc(ctx); err != nil {
			return err
		}
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

func requiresHTTPSSetup(cmd cli.Command) bool {
	return cmd.URLScheme == AutoURLScheme && !cmd.HTTP && !httpsConfigEnabled(router.DefaultStateDir())
}

func ensureHTTPSBrowserTrust(ctx context.Context, cmd cli.Command) error {
	if cmd.HTTP || cmd.URLScheme != AutoURLScheme || !detectWSLFunc() {
		return nil
	}
	stateDir := router.DefaultStateDir()
	if !httpsConfigEnabled(stateDir) {
		return nil
	}
	store := localcert.Store{StateDir: stateDir}
	trustFingerprint, err := store.TrustFingerprint()
	if err != nil || trustFingerprint == "" {
		return httpsBrowserTrustRepairError(err)
	}
	if ok, _ := windowsHTTPSCATrustedFunc(ctx, trustFingerprint); ok {
		return nil
	}
	if err := repairWindowsHTTPSTrustFunc(ctx, stateDir); err != nil {
		return httpsBrowserTrustRepairError(err)
	}
	if ok, detail := windowsHTTPSCATrustedFunc(ctx, trustFingerprint); !ok {
		return httpsBrowserTrustRepairError(errors.New(detail))
	}
	return nil
}

func httpsBrowserTrustRepairError(err error) error {
	if err == nil {
		err = errors.New("Windows browser trust is missing")
	}
	return fmt.Errorf("gohere error: HTTPS is enabled, but Windows browser trust is missing and could not be repaired.\nRun:\n  gohere doctor\n\nDetails: %w", err)
}

func httpsConfigEnabled(stateDir string) bool {
	cfg, err := appconfig.Load(stateDir)
	return err == nil && cfg.HTTPS
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
		return "gohere needs one-time permission to enable HTTPS .localhost project URLs.\nThis lets gohere use local HTTP/HTTPS ports and install a local trusted certificate authority. sudo access may be requested.\n\nContinue? [Y/n] "
	}
	return "gohere needs one-time permission to enable HTTPS .localhost project URLs.\nThis lets gohere use local HTTP/HTTPS ports and install a local trusted certificate authority.\n\nContinue? [Y/n] "
}

func shouldRunSetupFromAnswer(answer string) bool {
	answer = strings.TrimSpace(strings.ToLower(answer))
	return answer == "" || strings.HasPrefix(answer, "y")
}

func setupForGOOS(ctx context.Context, goos string) error {
	cfg := setup.Config{
		Stderr:   os.Stderr,
		HTTPS:    true,
		Progress: os.Stderr,
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
		if detectWSLFunc() {
			cfg.TrustCA = trustCAForWSL
		}
		return setupLinuxFunc(ctx, cfg)
	case "windows":
		return setupWindowsFunc(ctx, cfg)
	case "darwin":
		return setupDarwinFunc(ctx, cfg)
	default:
		return fmt.Errorf("gohere setup is not supported on %s yet", goos)
	}
}

type appCommandRunner struct{}

func (appCommandRunner) Run(ctx context.Context, command string, args ...string) error {
	cmd := execCommandContext(ctx, command, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func trustCAForWSL(ctx context.Context, caPath string) error {
	runner := appCommandRunner{}
	if err := certtrust.TrustCA(ctx, "linux", runner, caPath); err != nil {
		return err
	}
	windowsPath, err := wslWindowsPath(ctx, caPath)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Trusting certificate authority in Windows...")
	return runner.Run(ctx, "certutil.exe", "-f", "-user", "-addstore", "Root", windowsPath)
}

func repairWindowsHTTPSTrust(ctx context.Context, stateDir string) error {
	caPath := localcert.Store{StateDir: stateDir}.CACertPath()
	windowsPath, err := wslWindowsPath(ctx, caPath)
	if err != nil {
		return err
	}
	return appCommandRunner{}.Run(ctx, "certutil.exe", "-f", "-user", "-addstore", "Root", windowsPath)
}

func untrustCAForWSL(ctx context.Context, fingerprint string) error {
	runner := appCommandRunner{}
	if err := certtrust.UntrustCA(ctx, "linux", runner, fingerprint); err != nil {
		return err
	}
	return runner.Run(ctx, "certutil.exe", "-f", "-user", "-delstore", "Root", fingerprint)
}

func wslWindowsPath(ctx context.Context, path string) (string, error) {
	output, err := execCommandContext(ctx, "wslpath", "-w", path).Output()
	if err != nil {
		return "", commandOutputError("wslpath", output, err)
	}
	return strings.TrimSpace(string(output)), nil
}

type ListOptions struct {
	Verbose bool
	JSON    bool
}

type listRoute struct {
	ID             string `json:"id,omitempty"`
	Generation     uint64 `json:"generation,omitempty"`
	State          string `json:"state"`
	Service        string `json:"service,omitempty"`
	PreferredURL   string `json:"preferredUrl"`
	Port           int    `json:"port,omitempty"`
	Host           string `json:"host"`
	Target         string `json:"target"`
	Status         string `json:"status"`
	PID            int    `json:"pid"`
	CWD            string `json:"cwd"`
	Name           string `json:"name,omitempty"`
	Mode           string `json:"mode"`
	Source         string `json:"source"`
	OwnerEnv       string `json:"ownerEnv"`
	OwnerInstance  string `json:"ownerInstance,omitempty"`
	Distro         string `json:"distro,omitempty"`
	LinuxUser      string `json:"linuxUser,omitempty"`
	RunnerID       string `json:"runnerId,omitempty"`
	ListenTarget   string `json:"listenTarget,omitempty"`
	LeaseExpiresAt string `json:"leaseExpiresAt,omitempty"`
	ProjectRoot    string `json:"projectRoot,omitempty"`
	ProjectName    string `json:"projectName,omitempty"`
	StartedAt      string `json:"startedAt,omitempty"`
	CanStop        bool   `json:"canStop"`
	StopReason     string `json:"stopReason,omitempty"`
}

func List(ctx context.Context, stdout io.Writer, verbose bool) error {
	return ListWithOptions(ctx, stdout, ListOptions{Verbose: verbose})
}

func ListWithOptions(ctx context.Context, stdout io.Writer, opts ListOptions) error {
	if detectWSLFunc() {
		resolved, err := resolveWindowsCompanion(ctx, "control.route-statuses")
		if err != nil {
			return err
		}
		statuses, err := resolved.Control.RouteStatuses(ctx)
		if err != nil {
			return windowsCompanionUnavailableError(err)
		}
		return printRouteStatuses(stdout, convertAdminStatuses(statuses), opts)
	}
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
		if (route.OwnerEnv != "" || route.Source == "wsl") && !routeOwnedByCurrentEnv(route) {
			canStop = false
			stopReason = foreignRouteStopReason(route)
		}
		target := route.EffectiveTarget()
		port := 0
		if value, ok := routeTargetPort(target); ok {
			port, _ = strconv.Atoi(value)
		}
		routes = append(routes, listRoute{
			ID:             route.ID,
			Generation:     route.Generation,
			State:          string(route.EffectiveState()),
			Service:        route.Service,
			PreferredURL:   publicRouteURLForScheme(route.PreferredScheme, route.Host, route.URLPath),
			Port:           port,
			Host:           route.Host,
			Target:         target,
			Status:         string(status.Status),
			PID:            route.PID,
			CWD:            route.CWD,
			Name:           route.Name,
			Mode:           lifecycle.RouteMode(route),
			Source:         lifecycle.RouteSource(route),
			OwnerEnv:       lifecycle.RouteOwner(route),
			OwnerInstance:  route.OwnerInstance,
			Distro:         route.Distro,
			LinuxUser:      route.LinuxUser,
			RunnerID:       route.RunnerID,
			ListenTarget:   route.ListenTarget,
			LeaseExpiresAt: timeJSON(route.LeaseExpiresAt),
			ProjectRoot:    route.ProjectRoot,
			ProjectName:    route.ProjectName,
			StartedAt:      startedAtJSON(route),
			CanStop:        canStop,
			StopReason:     stopReason,
		})
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(routes)
}

func startedAtJSON(route router.Route) string {
	return timeJSON(route.StartedAt)
}

func timeJSON(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
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
	resolved, err := resolveWindowsCompanion(
		ctx,
		"control.delete-route",
		"control.route-statuses",
		"control.routes",
	)
	if err != nil {
		return routeManager{}, err
	}
	return routeManager{Client: resolved.Control, RouterReady: true}, nil
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
	now := time.Now()
	for _, status := range statuses {
		expiredReservation := status.Route.EffectiveState() == router.RouteStatePending && router.RouteReservationExpired(status.Route, now)
		expiredLease := status.Route.EffectiveState() == router.RouteStateActive && !status.Route.LeaseExpiresAt.IsZero() && router.RouteLeaseExpired(status.Route, now)
		if status.Status != lifecycle.RouteStatusDead && !expiredReservation && !expiredLease {
			continue
		}
		if status.Route.OwnerEnv == "wsl" && !expiredReservation && !router.RouteLeaseExpired(status.Route, now) {
			continue
		}
		if err := deleteAdminRouteIdentity(ctx, client, status.Route); err != nil {
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
	if owner == "" || owner != runOwnerEnv() {
		return false
	}
	if owner != "wsl" {
		return true
	}
	if route.OwnerInstance == "" || route.Distro == "" || route.LinuxUser == "" {
		return false
	}
	metadata, err := currentWSLMetadataFunc()
	if err != nil {
		return false
	}
	return route.OwnerInstance == metadata.OwnerInstance &&
		route.Distro == metadata.Distro &&
		route.LinuxUser == metadata.LinuxUser
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
		expiredActiveLease := status.Route.EffectiveState() == router.RouteStateActive &&
			!status.Route.LeaseExpiresAt.IsZero() && router.RouteLeaseExpired(status.Route, time.Now())
		if routeStatus == lifecycle.RouteStatusUnknown && routeOwnedByCurrentEnv(status.Route) && !expiredActiveLease {
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
		if err := deleteAdminRouteIdentity(ctx, client, route); err != nil {
			return result, err
		}
		result.Hosts = append(result.Hosts, route.Host)
	}
	return result, nil
}

func deleteAdminRouteIdentity(ctx context.Context, client adminClient, route router.Route) error {
	if route.ID != "" && route.Generation != 0 {
		if lifecycleClient, ok := client.(routeLifecycleClient); ok {
			if err := lifecycleClient.DeleteRouteRef(ctx, route.Ref()); err != nil {
				if errors.Is(err, admin.ErrUnauthorized) {
					return staleRouterTokenError()
				}
				if routeRefMismatch(err) {
					return nil
				}
				return err
			}
			return nil
		}
	}
	return deleteAdminRoute(ctx, client, route.Host)
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
	selectedRefs := make(map[router.RouteRef]bool, len(selected))
	selectedLegacyHosts := make(map[string]bool)
	for _, route := range selected {
		if route.ID != "" && route.Generation != 0 {
			selectedRefs[route.Ref()] = true
		} else {
			selectedLegacyHosts[route.Host] = true
		}
	}
	var result lifecycle.StopResult
	removeRefs := make(map[router.RouteRef]bool)
	removeLegacy := make(map[string]bool)
	for _, route := range routes {
		matched := selectedRefs[route.Ref()]
		if !matched && route.ID == "" {
			matched = selectedLegacyHosts[route.Host]
		}
		if !matched {
			continue
		}
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
		result.Hosts = append(result.Hosts, route.Host)
		if route.ID != "" && route.Generation != 0 {
			removeRefs[route.Ref()] = true
		} else {
			removeLegacy[appRouteUpdateKey(route)] = true
		}
	}
	if len(removeRefs) > 0 || len(removeLegacy) > 0 {
		if err := router.UpdateStore(store, func(routes []router.Route) ([]router.Route, error) {
			kept := routes[:0]
			for _, route := range routes {
				matched := removeRefs[route.Ref()]
				if !matched && len(removeLegacy) > 0 {
					matched = removeLegacy[appRouteUpdateKey(route)]
				}
				if matched {
					continue
				}
				kept = append(kept, route)
			}
			return kept, nil
		}); err != nil {
			return result, err
		}
	}
	return result, nil
}

func appRouteUpdateKey(route router.Route) string {
	return route.Host + "\x00" +
		route.Target + "\x00" +
		strconv.Itoa(route.PID) + "\x00" +
		route.ProcessIdentity + "\x00" +
		route.StartedAt.UTC().Format(time.RFC3339Nano)
}

type routeStopAction string

const (
	stopActionDelete    routeStopAction = "delete"
	stopActionTerminate routeStopAction = "terminate"
)

func stopAction(route router.Route) (routeStopAction, string) {
	if (route.OwnerEnv != "" || route.Source == "wsl") && !routeOwnedByCurrentEnv(route) {
		return "", foreignRouteStopReason(route)
	}
	if !lifecycle.PIDAlive(route.PID) || lifecycle.RouteStatuses([]router.Route{route})[0].Status == lifecycle.RouteStatusDead {
		return stopActionDelete, ""
	}
	if lifecycle.RouteProcessVerified(route) {
		return stopActionTerminate, ""
	}
	return "", "could not verify the original gohere process."
}

func foreignRouteStopReason(route router.Route) string {
	if route.OwnerEnv == "wsl" || route.Source == "wsl" {
		owner := strings.Trim(route.Distro+"/"+route.LinuxUser, "/")
		if owner == "" {
			owner = "another WSL owner"
		}
		return fmt.Sprintf("route belongs to %s; run gohere stop in that WSL owner.", owner)
	}
	return "route belongs to another environment."
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
		if (route.OwnerEnv != "" || route.Source == "wsl") && !routeOwnedByCurrentEnv(route) {
			continue
		}
		result.MatchedHost = route.Host
		if !lifecycle.PIDAlive(route.PID) || lifecycle.RouteStatuses([]router.Route{route})[0].Status == lifecycle.RouteStatusDead {
			if err := deleteAdminRouteIdentity(ctx, client, route); err != nil {
				return result, err
			}
			result.Hosts = append(result.Hosts, route.Host)
			continue
		}
		if lifecycle.RouteProcessVerified(route) {
			lifecycle.StopPID(route.PID)
			if err := deleteAdminRouteIdentity(ctx, client, route); err != nil {
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
	if detectWSLFunc() {
		resolved, err := resolveWindowsCompanion(ctx, "control.doctor")
		if err != nil {
			return err
		}
		output, err := resolved.Control.Doctor(ctx)
		if err != nil {
			return windowsCompanionUnavailableError(err)
		}
		fmt.Fprintln(stdout, "ok environment WSL")
		fmt.Fprintf(stdout, "ok router authority Windows companion %s\n", resolved.Info.CompanionVersion)
		fmt.Fprintf(stdout, "ok companion protocol %d\n", companion.ProtocolVersion)
		fmt.Fprintf(stdout, "ok router instance %s\n", resolved.Info.RouterInstanceID)
		transport, detail := inspectWSLPublicTransport(ctx, resolved.Info)
		if transport != "" {
			fmt.Fprintf(stdout, "ok public transport %s\n", transport)
		} else {
			fmt.Fprintf(stdout, "fail public transport unavailable: %s\n", detail)
		}
		if metadata, metadataErr := currentWSLMetadataFunc(); metadataErr == nil &&
			strings.EqualFold(metadata.CAFingerprint, resolved.Info.CAFingerprint) {
			fmt.Fprintf(stdout, "ok WSL CA fingerprint %s\n", metadata.CAFingerprint)
		} else if resolved.Info.CAFingerprint != "" {
			fmt.Fprintf(stdout, "fail WSL CA fingerprint does not match Windows %s\n", resolved.Info.CAFingerprint)
		}
		fmt.Fprint(stdout, output)
		return nil
	}
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
	usingWindowsBridge := goos == "linux" && wslWindowsServiceAvailable(ctx)
	checks := []lifecycle.DoctorCheck{
		{Name: "state dir", OK: exists(stateDir), Detail: stateDir, Hint: "Try: run gohere once to finish setup."},
		{Name: "stable binary", OK: exists(binaryPath), Detail: binaryPath, Hint: "Try: run gohere once to reinstall the local service binary."},
		{Name: "token", OK: exists(tokenPath), Detail: tokenPath, Hint: "Try: run gohere uninstall, then run gohere again."},
	}
	if exists(tokenPath) {
		if _, err := router.ReadToken(stateDir); err != nil {
			checks = append(checks, lifecycle.DoctorCheck{Name: "token format", OK: false, Detail: "invalid", Hint: "Try: gohere uninstall, then run gohere again."})
		} else {
			checks = append(checks, lifecycle.DoctorCheck{Name: "token format", OK: true, Detail: "valid"})
		}
	}
	if info, err := os.Stat(tokenPath); goos != "windows" && err == nil {
		checks = append(checks, lifecycle.DoctorCheck{Name: "token permissions", OK: info.Mode().Perm() == 0600, Detail: info.Mode().Perm().String(), Hint: "Try: chmod 600 ~/.gohere/token"})
	}
	adminHealthy := false
	if client != nil {
		adminHealthy = client.Health(ctx) == nil
		checks = append(checks, lifecycle.DoctorCheck{Name: "admin API health", OK: adminHealthy, Hint: "Try: gohere uninstall, then run gohere again."})
	} else {
		checks = append(checks, lifecycle.DoctorCheck{Name: "admin API health", OK: false, Detail: "unavailable", Hint: "Try: run gohere once to start the service."})
	}
	if usingWindowsBridge {
		checks = append(checks, lifecycle.DoctorCheck{Name: "local service", OK: true, Detail: "not required; using Windows service"})
	} else if pidData, err := os.ReadFile(pidPath); err == nil {
		pidText := strings.TrimSpace(string(pidData))
		checks = append(checks, lifecycle.DoctorCheck{Name: "service pid", OK: true, Detail: pidText})
		pid, err := strconv.Atoi(pidText)
		switch {
		case err != nil || pid <= 0:
			checks = append(checks, lifecycle.DoctorCheck{Name: "service process", OK: false, Detail: "invalid pid", Hint: "Try: gohere service stop, then run gohere again."})
		case lifecycle.PIDAlive(pid):
			checks = append(checks, lifecycle.DoctorCheck{Name: "service process", OK: true, Detail: "running"})
		default:
			checks = append(checks, lifecycle.DoctorCheck{Name: "service process", OK: false, Detail: "dead", Hint: "Try: run gohere once to restart the service."})
		}
	} else {
		checks = append(checks, lifecycle.DoctorCheck{Name: "service pid", OK: false, Detail: pidPath, Hint: "Try: run gohere once to start the service."})
	}
	if routes, err := store.Load(); err == nil {
		checks = append(checks, lifecycle.DoctorCheck{Name: "route store", OK: true, Detail: "valid"})
		checks = append(checks, lifecycle.DoctorCheck{Name: "active routes", OK: true, Detail: fmt.Sprintf("%d", len(routes))})
	} else {
		checks = append(checks, lifecycle.DoctorCheck{Name: "route store", OK: false, Detail: err.Error(), Hint: "Try: gohere prune or remove ~/.gohere/routes.json if it is corrupt."})
	}
	checks = append(checks, httpsDoctorChecks(ctx, stateDir)...)
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
	if goos == "linux" && exists(binaryPath) && !usingWindowsBridge {
		checks = append(checks, lifecycle.DoctorCheck{Name: "setcap", OK: extra.SetcapEnabled(binaryPath), Detail: "cap_net_bind_service", Hint: "Try: sudo setcap cap_net_bind_service=+ep ~/.gohere/bin/gohere"})
	}
	if goos == "linux" && !usingWindowsBridge {
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

func httpsDoctorCheck(stateDir string) lifecycle.DoctorCheck {
	cfg, err := appconfig.Load(stateDir)
	if err != nil {
		return lifecycle.DoctorCheck{Name: "https config", OK: false, Detail: "invalid", Hint: err.Error()}
	}
	if !cfg.HTTPS {
		return lifecycle.DoctorCheck{Name: "https config", OK: false, Detail: "disabled", Hint: "Run gohere again to enable HTTPS."}
	}
	fingerprint, err := localcert.Store{StateDir: stateDir}.Fingerprint()
	if err != nil || fingerprint == "" {
		return lifecycle.DoctorCheck{Name: "https certificate authority", OK: false, Detail: "missing", Hint: "Run gohere again to repair HTTPS setup."}
	}
	return lifecycle.DoctorCheck{Name: "https certificate authority", OK: true, Detail: fingerprint}
}

func httpsDoctorChecks(ctx context.Context, stateDir string) []lifecycle.DoctorCheck {
	check := httpsDoctorCheck(stateDir)
	checks := []lifecycle.DoctorCheck{check}
	if !check.OK || !detectWSLFunc() {
		return checks
	}
	trustFingerprint, err := localcert.Store{StateDir: stateDir}.TrustFingerprint()
	if err != nil || trustFingerprint == "" {
		checks = append(checks, lifecycle.DoctorCheck{Name: "windows https trust", OK: false, Detail: "missing", Hint: "Run gohere again to repair Windows browser trust."})
		return checks
	}
	ok, detail := windowsHTTPSCATrustedFunc(ctx, trustFingerprint)
	hint := ""
	if !ok {
		hint = "Run gohere again to repair Windows browser trust."
	}
	checks = append(checks, lifecycle.DoctorCheck{Name: "windows https trust", OK: ok, Detail: detail, Hint: hint})
	return checks
}

func wslWindowsServiceAvailable(ctx context.Context) bool {
	if !detectWSLFunc() {
		return false
	}
	_, err := resolveWindowsCompanion(ctx, "control.routes")
	return err == nil
}

func windowsHTTPSCATrusted(ctx context.Context, fingerprint string) (bool, string) {
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" {
		return false, "missing fingerprint"
	}
	output, err := execCommandContext(ctx, "certutil.exe", "-user", "-store", "Root", fingerprint).CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail == "" {
			detail = err.Error()
		}
		return false, compactDoctorDetail(detail)
	}
	return true, "trusted"
}

func bridgeDoctorChecks(ctx context.Context) []lifecycle.DoctorCheck {
	if !detectWSLFunc() {
		return nil
	}
	checks := []lifecycle.DoctorCheck{{Name: "environment", OK: true, Detail: "WSL"}}
	opened, err := openWindowsCompanion(ctx, "control.routes")
	if err != nil {
		return append(checks, lifecycle.DoctorCheck{
			Name:   "windows authority",
			OK:     false,
			Detail: compactDoctorDetail(err.Error()),
			Hint:   "Run gohere setup from this WSL shell.",
		})
	}
	if !opened.Info.RouterInstalled {
		return append(checks, lifecycle.DoctorCheck{Name: "windows service install", OK: false, Detail: "missing", Hint: "Run gohere setup from this WSL shell."})
	}
	resolved, err := resolveWindowsCompanion(ctx, "control.routes")
	if err != nil {
		return append(checks, lifecycle.DoctorCheck{Name: "windows service", OK: false, Detail: compactDoctorDetail(err.Error()), Hint: "Run gohere setup from this WSL shell."})
	}
	if _, err := resolved.Control.Routes(ctx); err != nil {
		return append(checks, lifecycle.DoctorCheck{Name: "windows service", OK: false, Detail: "unavailable: " + err.Error(), Hint: "Run gohere doctor again from this WSL shell."})
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
