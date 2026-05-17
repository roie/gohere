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
	got := runSuccessOutput("eventca", "eventca.localhost")
	want := "eventca is running\n\nhttp://eventca.localhost\n"
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
