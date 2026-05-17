package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/roie/gohere/internal/cli"
	"github.com/roie/gohere/internal/router"
	"github.com/roie/gohere/internal/runner"
)

func TestPrepareScriptRun(t *testing.T) {
	dir := tempProject(t, map[string]string{
		"package.json":   `{"scripts":{"dev":"vite --clearScreen false"}}`,
		"pnpm-lock.yaml": "",
	})

	plan, err := PrepareRun(cli.Command{Kind: cli.CommandRun, Script: "dev"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Port == 0 {
		t.Fatal("expected hidden port")
	}
	want := []string{"pnpm", "run", "dev", "--host", "127.0.0.1", "--port", itoa(plan.Port), "--strictPort"}
	if !sameStrings(plan.Command, want) {
		t.Fatalf("command = %#v, want %#v", plan.Command, want)
	}
	assertEnv(t, plan.Env, "PORT", itoa(plan.Port))
	assertEnv(t, plan.Env, "HOST", "127.0.0.1")
}

func TestPrepareRunUsesTargetPortOverride(t *testing.T) {
	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"dev":"next dev"}}`,
	})

	plan, err := PrepareRun(cli.Command{Kind: cli.CommandRun, Script: "dev", TargetPort: 5173}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Port != 5173 {
		t.Fatalf("port = %d, want 5173", plan.Port)
	}
	want := []string{"npm", "run", "dev", "--", "-p", "5173"}
	if !sameStrings(plan.Command, want) {
		t.Fatalf("command = %#v, want %#v", plan.Command, want)
	}
}

func TestPrepareRunUsesPortFlagEscapeHatch(t *testing.T) {
	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"dev":"custom-dev"}}`,
	})

	plan, err := PrepareRun(cli.Command{Kind: cli.CommandRun, Script: "dev", TargetPort: 5173, PortFlag: "--listen"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"npm", "run", "dev", "--", "--listen", "5173"}
	if !sameStrings(plan.Command, want) {
		t.Fatalf("command = %#v, want %#v", plan.Command, want)
	}
}

func TestPrepareRunMissingScriptShowsAvailableScripts(t *testing.T) {
	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"dev":"vite","dev:web":"vite"}}`,
	})

	_, err := PrepareRun(cli.Command{Kind: cli.CommandRun, Script: "preview"}, dir)
	if err == nil {
		t.Fatal("expected error")
	}
	if got := err.Error(); got != "script preview not found; available scripts: dev, dev:web" {
		t.Fatalf("error = %q", got)
	}
}

func TestPrepareRawCommandIsExact(t *testing.T) {
	dir := tempProject(t, nil)
	cmd := cli.Command{Kind: cli.CommandRaw, Raw: []string{"npm", "run", "dev", "--", "--host", "0.0.0.0"}, TargetPort: 5173}

	plan, err := PrepareRun(cmd, dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"npm", "run", "dev", "--", "--host", "0.0.0.0"}
	if !sameStrings(plan.Command, want) {
		t.Fatalf("command = %#v, want %#v", plan.Command, want)
	}
	assertEnv(t, plan.Env, "PORT", "5173")
	assertEnv(t, plan.Env, "HOST", "127.0.0.1")
}

func TestPrepareRawCommandRequiresTargetWhenNoPortDetected(t *testing.T) {
	dir := tempProject(t, nil)
	cmd := cli.Command{Kind: cli.CommandRaw, Raw: []string{"npm", "run", "dev"}}

	plan, err := PrepareRun(cmd, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.RequireDetectedPort {
		t.Fatalf("raw plan should require detected port without --target: %#v", plan)
	}
}

func TestPrepareRunFailsWithoutPackageJSON(t *testing.T) {
	dir := tempProject(t, nil)
	_, err := PrepareRun(cli.Command{Kind: cli.CommandRun, Script: "dev"}, dir)
	if err == nil {
		t.Fatal("expected error")
	}
	if got := err.Error(); got != "no package.json found; use gohere -- <command>" {
		t.Fatalf("error = %q", got)
	}
}

func TestPrepareStaticProject(t *testing.T) {
	dir := tempProject(t, map[string]string{"index.html": "<h1>Hello</h1>"})

	plan, err := PrepareRun(cli.Command{Kind: cli.CommandRun, Script: "dev"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Static {
		t.Fatal("expected static plan")
	}
	if plan.Host == "" || plan.Name == "" {
		t.Fatalf("plan = %#v", plan)
	}
}

func TestResolveRouteHostAvoidsActiveConflict(t *testing.T) {
	plan := RunPlan{
		Host: "myproject.localhost",
		CWD:  "/work/parent/myproject",
	}
	routes := []registeredRoute{
		{Host: "myproject.localhost", CWD: "/other/myproject"},
		{Host: "parent-myproject.localhost", CWD: "/other/parent-myproject"},
	}

	got := resolveRouteHost(plan, routes)
	if got != "parent-myproject-2.localhost" {
		t.Fatalf("resolveRouteHost() = %q", got)
	}
}

func TestRunSuccessOutput(t *testing.T) {
	got := runSuccessOutput(cli.Command{Kind: cli.CommandRun, Script: "dev"}, "eventca.localhost")
	want := "gohere \u2192 http://eventca.localhost\n"
	if got != want {
		t.Fatalf("runSuccessOutput() = %q, want %q", got, want)
	}
}

func TestRunSuccessOutputLabelsExplicitScript(t *testing.T) {
	got := runSuccessOutput(cli.Command{Kind: cli.CommandRun, Script: "dev:web"}, "eventca.localhost")
	want := "gohere dev:web \u2192 http://eventca.localhost\n"
	if got != want {
		t.Fatalf("runSuccessOutput() = %q, want %q", got, want)
	}
}

func TestRunSuccessOutputDoesNotLabelRawCommand(t *testing.T) {
	got := runSuccessOutput(cli.Command{Kind: cli.CommandRaw, Raw: []string{"npm", "run", "dev"}}, "eventca.localhost")
	want := "gohere \u2192 http://eventca.localhost\n"
	if got != want {
		t.Fatalf("runSuccessOutput() = %q, want %q", got, want)
	}
}

func TestShouldRunSetupFromAnswer(t *testing.T) {
	tests := map[string]bool{
		"\n":  true,
		"Y\n": true,
		"y\n": true,
		"n\n": false,
		"N\n": false,
	}

	for answer, want := range tests {
		t.Run(answer, func(t *testing.T) {
			got := shouldRunSetupFromAnswer(answer)
			if got != want {
				t.Fatalf("shouldRunSetupFromAnswer(%q) = %v, want %v", answer, got, want)
			}
		})
	}
}

func TestEnsureRouterPromptsAndRunsSetup(t *testing.T) {
	oldSetup := setupFunc
	oldPromptInput := promptInput
	defer func() {
		setupFunc = oldSetup
		promptInput = oldPromptInput
	}()

	calls := 0
	setupFunc = func(ctx context.Context) error {
		calls++
		return nil
	}
	promptInput = strings.NewReader("\n")
	var out strings.Builder

	err := ensureRouter(context.Background(), &out, func(context.Context) error {
		if calls == 0 {
			return errors.New("router unavailable")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("setup calls = %d, want 1", calls)
	}
	if !strings.Contains(out.String(), "Clean local URLs are not enabled yet.") {
		t.Fatalf("prompt output = %q", out.String())
	}
}

func TestRunEnsuresRouterBeforeStartingProject(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
	}()

	calls := []string{}
	defaultAdminClientFunc = func() (adminClient, error) {
		calls = append(calls, "admin")
		return fakeAdminClient{}, nil
	}
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		calls = append(calls, "runner")
		return nil, errors.New("stop after order check")
	}

	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"dev":"vite"}}`,
	})
	err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "dev"}, dir, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("expected runner error")
	}
	if len(calls) != 2 || calls[0] != "admin" || calls[1] != "runner" {
		t.Fatalf("calls = %#v", calls)
	}
}

func TestRunSuppressesChildOutputOnSuccessfulStartup(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
	}()

	defaultAdminClientFunc = func() (adminClient, error) {
		return fakeAdminClient{}, nil
	}
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		cfg.Stdout.Write([]byte("Local: http://127.0.0.1:5173/\n"))
		cfg.Stderr.Write([]byte("vite noisy warning\n"))
		return &runner.Result{Port: 5173}, nil
	}

	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"dev":"vite"}}`,
	})
	var stdout, stderr strings.Builder
	err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "dev"}, dir, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "127.0.0.1") || strings.Contains(stderr.String(), "vite noisy warning") {
		t.Fatalf("normal output leaked child startup logs, stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	want := "gohere \u2192 http://" + filepath.Base(dir) + ".localhost\n"
	if stdout.String() != want {
		t.Fatalf("normal output = %q, want %q", stdout.String(), want)
	}
}

func TestRunSuccessOutputLabelsExplicitScriptInNormalMode(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
	}()

	defaultAdminClientFunc = func() (adminClient, error) {
		return fakeAdminClient{}, nil
	}
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		cfg.Stdout.Write([]byte("Local: http://127.0.0.1:5173/\n"))
		return &runner.Result{Port: 5173}, nil
	}

	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"dev:web":"vite"}}`,
	})
	var stdout, stderr strings.Builder
	err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "dev:web"}, dir, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	want := "gohere dev:web \u2192 http://" + filepath.Base(dir) + ".localhost\n"
	if stdout.String() != want {
		t.Fatalf("normal script output = %q, want %q", stdout.String(), want)
	}
}

func TestRunStaticUsesPlainSuccessLabel(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
	}()

	admin := &recordingAdminClient{}
	defaultAdminClientFunc = func() (adminClient, error) {
		return admin, nil
	}
	dir := tempProject(t, map[string]string{"index.html": "<h1>Hello</h1>"})
	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr strings.Builder
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, cli.Command{Kind: cli.CommandRun, Script: "dev", TargetPort: 0}, dir, &stdout, &stderr)
	}()

	admin.waitForUpsert(t)
	cancel()

	if err := <-done; err != nil {
		t.Fatal(err)
	}
	want := "gohere \u2192 http://" + filepath.Base(dir) + ".localhost\n"
	if stdout.String() != want {
		t.Fatalf("static output = %q, want %q", stdout.String(), want)
	}
}

func TestRunReplaysChildOutputOnStartupFailure(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
	}()

	defaultAdminClientFunc = func() (adminClient, error) {
		return fakeAdminClient{}, nil
	}
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		cfg.Stdout.Write([]byte("starting dev server\n"))
		cfg.Stderr.Write([]byte("Error: config is invalid\n"))
		return nil, errors.New("started dev script, but could not detect a local URL; try: gohere --target 5173")
	}

	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"dev":"vite"}}`,
	})
	var stdout, stderr strings.Builder
	err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "dev"}, dir, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected startup error")
	}
	if !strings.Contains(stderr.String(), "starting dev server") || !strings.Contains(stderr.String(), "Error: config is invalid") {
		t.Fatalf("startup failure should replay child output, stderr=%q", stderr.String())
	}
	wantErr := "gohere error: started dev script, but could not detect a local URL.\nTry:\n  gohere --target 5173"
	if err.Error() != wantErr {
		t.Fatalf("error = %q, want %q", err.Error(), wantErr)
	}
}

func TestRunVerboseOutputIncludesCleanURLAndMetadata(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
	}()

	defaultAdminClientFunc = func() (adminClient, error) {
		return fakeAdminClient{}, nil
	}
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		cfg.Stdout.Write([]byte("Local: http://127.0.0.1:5173/\n"))
		return &runner.Result{Port: 5173}, nil
	}

	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"dev":"vite"}}`,
	})
	var stdout, stderr strings.Builder
	err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "dev", Verbose: true}, dir, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stdout.String(), "gohere \u2192 http://"+filepath.Base(dir)+".localhost\n\n") ||
		!strings.Contains(stdout.String(), "\ntarget: http://127.0.0.1:5173\n") ||
		!strings.Contains(stdout.String(), "command: npm run dev -- --host 127.0.0.1 --port ") ||
		!strings.Contains(stdout.String(), "router: running\n") {
		t.Fatalf("verbose stdout = %q", stdout.String())
	}
}

func TestRunTreatsStartupContextCancelAsCleanShutdown(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
	}()

	defaultAdminClientFunc = func() (adminClient, error) {
		return fakeAdminClient{}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		cancel()
		return nil, context.Canceled
	}

	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"dev":"vite"}}`,
	})
	if err := Run(ctx, cli.Command{Kind: cli.CommandRun, Script: "dev"}, dir, io.Discard, io.Discard); err != nil {
		t.Fatalf("Run after startup context cancel = %v, want nil", err)
	}
}

func TestRunStaticTreatsContextCancelAsCleanShutdown(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
	}()

	admin := &recordingAdminClient{}
	defaultAdminClientFunc = func() (adminClient, error) {
		return admin, nil
	}
	dir := tempProject(t, map[string]string{"index.html": "<h1>Hello</h1>"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, cli.Command{Kind: cli.CommandRun, Script: "dev", TargetPort: 0}, dir, io.Discard, io.Discard)
	}()

	admin.waitForUpsert(t)
	cancel()

	if err := <-done; err != nil {
		t.Fatalf("Run static after context cancel = %v, want nil", err)
	}
	if admin.deleted == "" {
		t.Fatal("expected static route cleanup")
	}
}

func TestDoctorWithStoreReportsActiveRouteCount(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, "router.pid"), []byte("12345\n"), 0600); err != nil {
		t.Fatal(err)
	}
	store := router.NewMemoryStore()
	store.Save([]router.Route{{Host: "app.localhost", Target: "http://127.0.0.1:1234"}})
	var out strings.Builder

	if err := DoctorWithStore(&out, stateDir, store, fakeAdminClient{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ok active routes 1") {
		t.Fatalf("doctor output = %q", out.String())
	}
	if !strings.Contains(out.String(), "ok router pid 12345") {
		t.Fatalf("doctor output = %q", out.String())
	}
}

func TestDoctorWithStoreReportsPort80Availability(t *testing.T) {
	stateDir := t.TempDir()
	store := router.NewMemoryStore()
	var out strings.Builder

	if err := DoctorWithChecks(&out, stateDir, store, fakeAdminClient{}, DoctorChecks{Port80Available: func() bool {
		return true
	}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ok port 80 available") {
		t.Fatalf("doctor output = %q", out.String())
	}
}

func TestDoctorWithStoreTreatsHealthyRouterAsPort80OK(t *testing.T) {
	var out strings.Builder

	if err := DoctorWithChecks(&out, t.TempDir(), router.NewMemoryStore(), fakeAdminClient{}, DoctorChecks{Port80Available: func() bool {
		return false
	}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ok port 80 used by gohere router") {
		t.Fatalf("doctor output = %q", out.String())
	}
}

func TestDoctorWithStoreReportsSetcapStatus(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stateDir, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	binaryPath := filepath.Join(stateDir, "bin", "gohere")
	if err := os.WriteFile(binaryPath, []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder

	if err := DoctorWithChecks(&out, stateDir, router.NewMemoryStore(), fakeAdminClient{}, DoctorChecks{
		SetcapEnabled: func(path string) bool {
			return path == binaryPath
		},
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ok setcap cap_net_bind_service") {
		t.Fatalf("doctor output = %q", out.String())
	}
}

func TestDoctorWithStoreReportsSystemdStatus(t *testing.T) {
	var out strings.Builder

	if err := DoctorWithChecks(&out, t.TempDir(), router.NewMemoryStore(), fakeAdminClient{}, DoctorChecks{
		SystemdUserServiceOK: func() (bool, bool) {
			return true, true
		},
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ok systemd user service active") {
		t.Fatalf("doctor output = %q", out.String())
	}
}

func TestSystemdUserAvailableRequiresUserBus(t *testing.T) {
	runUserRoot := t.TempDir()

	if systemdUserAvailableAt(runUserRoot, 1000) {
		t.Fatal("systemd user should be unavailable without user bus")
	}
	if err := os.MkdirAll(filepath.Join(runUserRoot, "1000"), 0755); err != nil {
		t.Fatal(err)
	}
	if systemdUserAvailableAt(runUserRoot, 1000) {
		t.Fatal("systemd user should be unavailable without bus socket")
	}
	if err := os.WriteFile(filepath.Join(runUserRoot, "1000", "bus"), []byte{}, 0600); err != nil {
		t.Fatal(err)
	}
	if !systemdUserAvailableAt(runUserRoot, 1000) {
		t.Fatal("systemd user should be available with user bus")
	}
}

type fakeAdminClient struct{}

func (fakeAdminClient) Health(context.Context) error {
	return nil
}

func (fakeAdminClient) Routes(context.Context) ([]router.Route, error) {
	return nil, nil
}

func (fakeAdminClient) UpsertRoute(context.Context, router.Route) error {
	return nil
}

func (fakeAdminClient) DeleteRoute(context.Context, string) error {
	return nil
}

type recordingAdminClient struct {
	upserted chan struct{}
	deleted  string
}

func (c *recordingAdminClient) Health(context.Context) error {
	return nil
}

func (c *recordingAdminClient) Routes(context.Context) ([]router.Route, error) {
	return nil, nil
}

func (c *recordingAdminClient) UpsertRoute(ctx context.Context, route router.Route) error {
	if c.upserted == nil {
		c.upserted = make(chan struct{})
	}
	close(c.upserted)
	return nil
}

func (c *recordingAdminClient) DeleteRoute(ctx context.Context, host string) error {
	c.deleted = host
	return nil
}

func (c *recordingAdminClient) waitForUpsert(t *testing.T) {
	t.Helper()
	if c.upserted == nil {
		c.upserted = make(chan struct{})
	}
	select {
	case <-c.upserted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for route registration")
	}
}

func tempProject(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, contents := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func assertEnv(t *testing.T, env []string, key, want string) {
	t.Helper()
	prefix := key + "="
	for _, item := range env {
		if len(item) >= len(prefix) && item[:len(prefix)] == prefix {
			if got := item[len(prefix):]; got != want {
				t.Fatalf("%s = %q, want %q", key, got, want)
			}
			return
		}
	}
	t.Fatalf("missing env %s", key)
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	out := ""
	for n > 0 {
		out = string(rune('0'+n%10)) + out
		n /= 10
	}
	return out
}
