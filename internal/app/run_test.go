package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/roie/gohere/internal/admin"
	"github.com/roie/gohere/internal/bridge"
	"github.com/roie/gohere/internal/cli"
	"github.com/roie/gohere/internal/router"
	"github.com/roie/gohere/internal/runner"
	"github.com/roie/gohere/internal/setup"
)

func TestMain(m *testing.M) {
	detectWSLFunc = func() bool { return false }
	os.Exit(m.Run())
}

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
	if plan.ProjectRoot != dir {
		t.Fatalf("project root = %q, want %q", plan.ProjectRoot, dir)
	}
	want := []string{"pnpm", "run", "dev", "--host", "127.0.0.1", "--port", itoa(plan.Port), "--strictPort"}
	if !sameStrings(plan.Command, want) {
		t.Fatalf("command = %#v, want %#v", plan.Command, want)
	}
	assertEnv(t, plan.Env, "PORT", itoa(plan.Port))
	assertEnv(t, plan.Env, "HOST", "127.0.0.1")
}

func TestDefaultAdminClientDoesNotCreateMissingToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	_, err := defaultAdminClient()
	if err == nil {
		t.Fatal("expected missing token error")
	}
	if _, statErr := os.Stat(filepath.Join(home, ".gohere", "token")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("token file stat err = %v, want not exist", statErr)
	}
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
	if got := err.Error(); got != "gohere error: script \"preview\" not found; available scripts: dev, dev:web" {
		t.Fatalf("error = %q", got)
	}
}

func TestPrepareRunMissingScriptShowsLongScriptListMultiline(t *testing.T) {
	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"build":"vite build","dev":"vite","dev:web":"vite","test":"vitest"}}`,
	})

	_, err := PrepareRun(cli.Command{Kind: cli.CommandRun, Script: "preview"}, dir)
	if err == nil {
		t.Fatal("expected error")
	}
	want := "gohere error: script \"preview\" not found.\n\nAvailable scripts:\n  build\n  dev\n  dev:web\n  test"
	if got := err.Error(); got != want {
		t.Fatalf("error = %q, want %q", got, want)
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
	want := "No package.json or index.html found; use gohere -- <command>."
	if got := err.Error(); got != want {
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

func TestPrepareStaticProjectWinsOverParentPackageForDefaultRun(t *testing.T) {
	root := tempProject(t, map[string]string{
		"package.json": `{"scripts":{}}`,
	})
	dir := filepath.Join(root, "site")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>Hello</h1>"), 0600); err != nil {
		t.Fatal(err)
	}

	plan, err := PrepareRun(cli.Command{Kind: cli.CommandRun, Script: "dev"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Static {
		t.Fatalf("expected static plan, got %#v", plan)
	}
}

func TestPreparePackageProjectWinsOverStaticIndexForDefaultRun(t *testing.T) {
	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"dev":"vite --clearScreen false"}}`,
		"index.html":   "<div id=\"root\"></div>",
	})

	plan, err := PrepareRun(cli.Command{Kind: cli.CommandRun, Script: "dev"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Static {
		t.Fatalf("expected package script plan, got static: %#v", plan)
	}
	if len(plan.Command) == 0 || plan.Command[0] != "npm" {
		t.Fatalf("command = %#v, want npm script command", plan.Command)
	}
}

func TestPrepareStaticFileTarget(t *testing.T) {
	dir := tempProject(t, map[string]string{
		"index.html":        "<h1>Hello</h1>",
		"pages/about.html":  "<h1>About</h1>",
		"assets/styles.css": "body{}",
	})

	plan, err := PrepareRun(cli.Command{Kind: cli.CommandRun, Script: "pages/about.html"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Static || plan.URLPath != "/pages/about.html" {
		t.Fatalf("plan = %#v", plan)
	}

	plan, err = PrepareRun(cli.Command{Kind: cli.CommandRun, Script: "assets/styles.css"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Static || plan.URLPath != "/assets/styles.css" {
		t.Fatalf("plan = %#v", plan)
	}
}

func TestPrepareStaticFileTargetWinsOverPackageScriptWhenFileExists(t *testing.T) {
	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"about.html":"vite"}}`,
		"about.html":   "<h1>About</h1>",
	})

	plan, err := PrepareRun(cli.Command{Kind: cli.CommandRun, Script: "about.html"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Static || plan.URLPath != "/about.html" || len(plan.Command) != 0 {
		t.Fatalf("plan = %#v", plan)
	}
}

func TestPrepareStaticFileTargetWinsOverParentPackageHostname(t *testing.T) {
	root := tempProject(t, map[string]string{
		"package.json": `{"name":"parent-package","scripts":{}}`,
	})
	dir := filepath.Join(root, "site")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "about.html"), []byte("<h1>About</h1>"), 0600); err != nil {
		t.Fatal(err)
	}

	plan, err := PrepareRun(cli.Command{Kind: cli.CommandRun, Script: "about.html"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Host != "site.localhost" {
		t.Fatalf("host = %q, want site.localhost", plan.Host)
	}
}

func TestPrepareFileTargetMissingDoesNotFallBackToPackageScript(t *testing.T) {
	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"about.html":"vite"}}`,
	})

	_, err := PrepareRun(cli.Command{Kind: cli.CommandRun, Script: "about.html", TargetPort: 5173}, dir)
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Error() != "File not found: about.html" {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestPrepareStaticFileTargetErrors(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		arg   string
		want  string
	}{
		{
			name:  "missing extension file",
			files: map[string]string{"index.html": "<h1>Hello</h1>"},
			arg:   "missing.html",
			want:  "File not found: missing.html",
		},
		{
			name:  "extensionless static arg",
			files: map[string]string{"index.html": "<h1>Hello</h1>"},
			arg:   "pages",
			want:  "Static files need a file extension: pages",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := tempProject(t, tt.files)
			_, err := PrepareRun(cli.Command{Kind: cli.CommandRun, Script: tt.arg}, dir)
			if err == nil {
				t.Fatal("expected error")
			}
			if err.Error() != tt.want {
				t.Fatalf("error = %q, want %q", err.Error(), tt.want)
			}
		})
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
	got := runSuccessOutput(cli.Command{Kind: cli.CommandRun, Script: "dev"}, "eventca.localhost", "")
	want := "gohere \u2192 http://eventca.localhost\n"
	if got != want {
		t.Fatalf("runSuccessOutput() = %q, want %q", got, want)
	}
}

func TestRunSuccessOutputLabelsExplicitScript(t *testing.T) {
	got := runSuccessOutput(cli.Command{Kind: cli.CommandRun, Script: "dev:web"}, "eventca.localhost", "")
	want := "gohere dev:web \u2192 http://eventca.localhost\n"
	if got != want {
		t.Fatalf("runSuccessOutput() = %q, want %q", got, want)
	}
}

func TestRunSuccessOutputLabelsStaticFileTarget(t *testing.T) {
	got := runSuccessOutput(cli.Command{Kind: cli.CommandRun, Script: "pages/about.html"}, "eventca.localhost", "/pages/about.html")
	want := "gohere pages/about.html \u2192 http://eventca.localhost/pages/about.html\n"
	if got != want {
		t.Fatalf("runSuccessOutput() = %q, want %q", got, want)
	}
}

func TestRunSuccessOutputDoesNotLabelRawCommand(t *testing.T) {
	got := runSuccessOutput(cli.Command{Kind: cli.CommandRaw, Raw: []string{"npm", "run", "dev"}}, "eventca.localhost", "")
	want := "gohere \u2192 http://eventca.localhost\n"
	if got != want {
		t.Fatalf("runSuccessOutput() = %q, want %q", got, want)
	}
}

func TestShouldRunSetupFromAnswer(t *testing.T) {
	tests := map[string]bool{
		"\n":    true,
		"Y\n":   true,
		"y\n":   true,
		"yes\n": true,
		"yep\n": true,
		"ye\n":  true,
		"n\n":   false,
		"N\n":   false,
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
	oldStartInstalledRouter := startInstalledRouterFunc
	defer func() {
		setupFunc = oldSetup
		promptInput = oldPromptInput
		startInstalledRouterFunc = oldStartInstalledRouter
	}()

	calls := 0
	setupFunc = func(ctx context.Context) error {
		calls++
		return nil
	}
	startInstalledRouterFunc = func(context.Context) error {
		return os.ErrNotExist
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
	want := "gohere needs one-time permission to enable .localhost project URLs.\nThis lets gohere use port 80 locally. Continue? [Y/n] \n"
	if out.String() != want {
		t.Fatalf("prompt output = %q, want %q", out.String(), want)
	}
}

func TestEnsureRouterRestartsInstalledRouterWithoutPrompt(t *testing.T) {
	oldSetup := setupFunc
	oldPromptInput := promptInput
	oldStartInstalledRouter := startInstalledRouterFunc
	defer func() {
		setupFunc = oldSetup
		promptInput = oldPromptInput
		startInstalledRouterFunc = oldStartInstalledRouter
	}()

	setupFunc = func(ctx context.Context) error {
		t.Fatal("setup should not run when installed router restarts")
		return nil
	}
	promptInput = strings.NewReader("n\n")
	restarted := false
	startInstalledRouterFunc = func(context.Context) error {
		restarted = true
		return nil
	}
	healthCalls := 0
	var out strings.Builder

	err := ensureRouter(context.Background(), &out, func(context.Context) error {
		healthCalls++
		if !restarted {
			return errors.New("router unavailable")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !restarted {
		t.Fatal("installed router was not restarted")
	}
	if out.String() != "" {
		t.Fatalf("prompt output = %q, want empty", out.String())
	}
}

func TestEnsureRouterDoesNotPromptWhenInstalledRouterRestartFails(t *testing.T) {
	oldSetup := setupFunc
	oldPromptInput := promptInput
	oldStartInstalledRouter := startInstalledRouterFunc
	defer func() {
		setupFunc = oldSetup
		promptInput = oldPromptInput
		startInstalledRouterFunc = oldStartInstalledRouter
	}()

	setupFunc = func(ctx context.Context) error {
		t.Fatal("setup should not run when installed router restart fails")
		return nil
	}
	promptInput = strings.NewReader("y\n")
	startInstalledCalls := 0
	startInstalledRouterFunc = func(context.Context) error {
		startInstalledCalls++
		return errors.New("port already in use")
	}
	var out strings.Builder

	err := ensureRouter(context.Background(), &out, func(context.Context) error {
		return errors.New("router unavailable")
	})
	if err == nil {
		t.Fatal("expected installed router restart error")
	}
	if !strings.Contains(err.Error(), "installed gohere service is not reachable") {
		t.Fatalf("error = %q", err.Error())
	}
	if !strings.Contains(err.Error(), "gohere doctor") {
		t.Fatalf("error should point to doctor, got %q", err.Error())
	}
	if startInstalledCalls != 1 {
		t.Fatalf("installed router restart calls = %d, want 1", startInstalledCalls)
	}
	if out.String() != "" {
		t.Fatalf("prompt output = %q, want empty", out.String())
	}
}

func TestEnsureRouterDeclinePrintsCalmMessage(t *testing.T) {
	oldSetup := setupFunc
	oldPromptInput := promptInput
	oldStartInstalledRouter := startInstalledRouterFunc
	defer func() {
		setupFunc = oldSetup
		promptInput = oldPromptInput
		startInstalledRouterFunc = oldStartInstalledRouter
	}()

	setupFunc = func(ctx context.Context) error {
		t.Fatal("setup should not run after decline")
		return nil
	}
	startInstalledRouterFunc = func(context.Context) error {
		return os.ErrNotExist
	}
	promptInput = strings.NewReader("n\n")
	var out strings.Builder

	err := ensureRouter(context.Background(), &out, func(context.Context) error {
		return errors.New("router unavailable")
	})
	if err == nil {
		t.Fatal("expected decline error")
	}
	if err.Error() != "gohere was not enabled" {
		t.Fatalf("error = %q", err.Error())
	}
	want := "gohere needs one-time permission to enable .localhost project URLs.\nThis lets gohere use port 80 locally. Continue? [Y/n] gohere was not enabled.\n\nRun gohere again when you are ready.\n"
	if out.String() != want {
		t.Fatalf("decline output = %q, want %q", out.String(), want)
	}
}

func TestEnsureRouterAddsBlankLineAfterSetup(t *testing.T) {
	oldSetup := setupFunc
	oldPromptInput := promptInput
	oldStartInstalledRouter := startInstalledRouterFunc
	defer func() {
		setupFunc = oldSetup
		promptInput = oldPromptInput
		startInstalledRouterFunc = oldStartInstalledRouter
	}()

	calls := 0
	setupFunc = func(ctx context.Context) error {
		calls++
		return nil
	}
	startInstalledRouterFunc = func(context.Context) error {
		return os.ErrNotExist
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
	if !strings.HasSuffix(out.String(), "\n") {
		t.Fatalf("setup output should end with blank separator, got %q", out.String())
	}
}

func TestEnsureRouterWaitsForRouterAfterSetup(t *testing.T) {
	oldSetup := setupFunc
	oldPromptInput := promptInput
	oldStartInstalledRouter := startInstalledRouterFunc
	defer func() {
		setupFunc = oldSetup
		promptInput = oldPromptInput
		startInstalledRouterFunc = oldStartInstalledRouter
	}()

	setupFunc = func(ctx context.Context) error {
		return nil
	}
	startInstalledRouterFunc = func(context.Context) error {
		return os.ErrNotExist
	}
	promptInput = strings.NewReader("\n")
	healthCalls := 0
	var out strings.Builder

	err := ensureRouter(context.Background(), &out, func(context.Context) error {
		healthCalls++
		if healthCalls < 3 {
			return errors.New("router still starting")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if healthCalls != 3 {
		t.Fatalf("health calls = %d, want 3", healthCalls)
	}
}

func TestSetupForGOOSUsesWindowsSetup(t *testing.T) {
	oldSetupWindows := setupWindowsFunc
	defer func() {
		setupWindowsFunc = oldSetupWindows
	}()
	calls := 0
	setupWindowsFunc = func(ctx context.Context, cfg setup.Config) error {
		calls++
		return nil
	}

	if err := setupForGOOS(context.Background(), "windows"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("windows setup calls = %d, want 1", calls)
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

func TestRunReportsStaleRouterToken(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
	}()

	defaultAdminClientFunc = func() (adminClient, error) {
		return staleTokenAdminClient{}, nil
	}
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		return &runner.Result{Port: 5173}, nil
	}
	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"dev":"vite"}}`,
	})

	err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "dev"}, dir, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("expected stale router token error")
	}
	if !strings.Contains(err.Error(), "A gohere service is already using .localhost URLs") {
		t.Fatalf("error = %q", err.Error())
	}
	if !strings.Contains(err.Error(), "gohere service stop") {
		t.Fatalf("error should recommend gohere service stop: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "Windows and WSL") {
		t.Fatalf("error should mention Windows/WSL split: %q", err.Error())
	}
	if strings.Contains(err.Error(), "systemctl") {
		t.Fatalf("error should not lead with systemd internals: %q", err.Error())
	}
	if strings.Contains(err.Error(), "GET /routes returned 401") {
		t.Fatalf("error leaked raw admin API response: %q", err.Error())
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

func TestRunOpenLaunchesBrowserAfterRouteRegistration(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	oldOpenBrowser := openBrowserFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
		openBrowserFunc = oldOpenBrowser
	}()

	defaultAdminClientFunc = func() (adminClient, error) {
		return fakeAdminClient{}, nil
	}
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		return &runner.Result{Port: 5173}, nil
	}
	var opened string
	openBrowserFunc = func(ctx context.Context, url string) error {
		opened = url
		return nil
	}

	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"dev":"vite"}}`,
	})
	var stdout, stderr strings.Builder
	err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "dev", Open: true}, dir, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	wantURL := "http://" + filepath.Base(dir) + ".localhost"
	if opened != wantURL {
		t.Fatalf("opened = %q, want %q", opened, wantURL)
	}
	if stdout.String() != "gohere \u2192 "+wantURL+"\n" || stderr.String() != "" {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestRunOpenFailureKeepsServerRunning(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	oldOpenBrowser := openBrowserFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
		openBrowserFunc = oldOpenBrowser
	}()

	defaultAdminClientFunc = func() (adminClient, error) {
		return fakeAdminClient{}, nil
	}
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		return &runner.Result{Port: 5173}, nil
	}
	openBrowserFunc = func(ctx context.Context, url string) error {
		return errors.New("open failed")
	}

	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"dev":"vite"}}`,
	})
	var stdout, stderr strings.Builder
	err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "dev", Open: true}, dir, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	wantURL := "http://" + filepath.Base(dir) + ".localhost"
	if !strings.Contains(stderr.String(), "Could not open browser automatically.\nOpen manually: "+wantURL+"\n") {
		t.Fatalf("stderr = %q", stderr.String())
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
	if admin.route.PID == 0 {
		t.Fatalf("static route PID = 0, want current gohere process PID")
	}
	want := "gohere \u2192 http://" + filepath.Base(dir) + ".localhost\n"
	if stdout.String() != want {
		t.Fatalf("static output = %q, want %q", stdout.String(), want)
	}
}

func TestRunStaticFileTargetOutput(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
	}()

	admin := &recordingAdminClient{}
	defaultAdminClientFunc = func() (adminClient, error) {
		return admin, nil
	}
	dir := tempProject(t, map[string]string{
		"index.html":       "<h1>Hello</h1>",
		"pages/about.html": "<h1>About</h1>",
	})
	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr strings.Builder
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, cli.Command{Kind: cli.CommandRun, Script: "pages/about.html", TargetPort: 0}, dir, &stdout, &stderr)
	}()

	admin.waitForUpsert(t)
	cancel()

	if err := <-done; err != nil {
		t.Fatal(err)
	}
	want := "gohere pages/about.html \u2192 http://" + filepath.Base(dir) + ".localhost/pages/about.html\n"
	if stdout.String() != want {
		t.Fatalf("static file output = %q, want %q", stdout.String(), want)
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
		!strings.Contains(stdout.String(), "project root: "+dir+"\n") ||
		!strings.Contains(stdout.String(), "command: npm run dev -- --host 127.0.0.1 --port ") ||
		!strings.Contains(stdout.String(), "service: running\n") {
		t.Fatalf("verbose stdout = %q", stdout.String())
	}
}

func TestRunUsesWindowsRouterBridgeFromWSL(t *testing.T) {
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:         true,
		token:         "windows-token",
		wslIP:         "172.20.10.2",
		reachable:     true,
		windowsBinary: true,
		admin:         &recordingAdminClient{},
		localAdmin:    fakeAdminClient{},
	})
	defer restore()

	oldStartRunner := startRunnerFunc
	defer func() {
		startRunnerFunc = oldStartRunner
	}()
	var gotEnv []string
	var gotCommand []string
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		gotEnv = cfg.Env
		gotCommand = cfg.Command
		return &runner.Result{Port: 5173}, nil
	}

	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"dev":"vite"}}`,
	})
	var stdout strings.Builder
	if err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "dev", Verbose: true}, dir, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}

	assertEnv(t, gotEnv, "HOST", "0.0.0.0")
	if !strings.Contains(strings.Join(gotCommand, " "), "--host 0.0.0.0") {
		t.Fatalf("command = %#v, want bridge host injection", gotCommand)
	}
	if !strings.Contains(stdout.String(), "\ntarget: http://127.0.0.1:5173\n") ||
		!strings.Contains(stdout.String(), "service: Windows\n") {
		t.Fatalf("verbose stdout = %q", stdout.String())
	}
}

func TestResolveRunRouterFallsBackWhenWindowsRouterAbsent(t *testing.T) {
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:      true,
		healthErr:  errors.New("connection refused"),
		tokenErr:   bridge.ErrWindowsTokenNotFound,
		localAdmin: fakeAdminClient{},
	})
	defer restore()

	runRouter, err := resolveRunRouter(context.Background(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if runRouter.RouterLabel != "running" || runRouter.ChildHost != "127.0.0.1" || runRouter.RouteTargetHost != "127.0.0.1" {
		t.Fatalf("runRouter = %#v", runRouter)
	}
}

func TestResolveRunRouterStopsWhenWindowsRouterInstalledButNotRunning(t *testing.T) {
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:         true,
		token:         "windows-token",
		healthErr:     errors.New("connection refused"),
		windowsBinary: true,
	})
	defer restore()

	_, err := resolveRunRouter(context.Background(), io.Discard)
	if err == nil {
		t.Fatal("expected windows router unavailable error")
	}
	if !strings.Contains(err.Error(), "Windows gohere is installed, but its service is not running") {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestResolveRunRouterFallsBackWhenOnlyWindowsTokenRemains(t *testing.T) {
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:      true,
		token:      "windows-token",
		healthErr:  errors.New("connection refused"),
		localAdmin: fakeAdminClient{},
	})
	defer restore()

	runRouter, err := resolveRunRouter(context.Background(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if runRouter.RouterLabel != "running" || runRouter.ChildHost != "127.0.0.1" || runRouter.RouteTargetHost != "127.0.0.1" {
		t.Fatalf("runRouter = %#v", runRouter)
	}
}

func TestResolveRunRouterFallsBackWhenStaleWindowsTokenSeesWSLLocalRouter(t *testing.T) {
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:      true,
		token:      "windows-token",
		healthErr:  nil,
		localAdmin: fakeAdminClient{},
	})
	defer restore()

	runRouter, err := resolveRunRouter(context.Background(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if runRouter.RouterLabel != "running" || runRouter.ChildHost != "127.0.0.1" || runRouter.RouteTargetHost != "127.0.0.1" {
		t.Fatalf("runRouter = %#v", runRouter)
	}
}

func TestResolveRunRouterRunsSetupBeforeReadingMissingToken(t *testing.T) {
	oldDetectWSL := detectWSLFunc
	oldDefaultAdminClient := defaultAdminClientFunc
	oldRouterHealth := routerHealthFunc
	oldSetup := setupFunc
	oldStartInstalledRouter := startInstalledRouterFunc
	oldPromptInput := promptInput
	defer func() {
		detectWSLFunc = oldDetectWSL
		defaultAdminClientFunc = oldDefaultAdminClient
		routerHealthFunc = oldRouterHealth
		setupFunc = oldSetup
		startInstalledRouterFunc = oldStartInstalledRouter
		promptInput = oldPromptInput
	}()

	detectWSLFunc = func() bool { return false }
	setupDone := false
	defaultAdminClientFunc = func() (adminClient, error) {
		if !setupDone {
			return nil, os.ErrNotExist
		}
		return fakeAdminClient{}, nil
	}
	routerHealthFunc = func(context.Context) error {
		if !setupDone {
			return errors.New("router unavailable")
		}
		return nil
	}
	setupFunc = func(context.Context) error {
		setupDone = true
		return nil
	}
	startInstalledRouterFunc = func(context.Context) error {
		return os.ErrNotExist
	}
	promptInput = strings.NewReader("\n")

	runRouter, err := resolveRunRouter(context.Background(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if runRouter.Client == nil || runRouter.RouteTargetHost != "127.0.0.1" {
		t.Fatalf("runRouter = %#v", runRouter)
	}
}

func TestResolveRunRouterReportsUncontrolledRouterWhenTokenMissingAfterHealth(t *testing.T) {
	oldDetectWSL := detectWSLFunc
	oldDefaultAdminClient := defaultAdminClientFunc
	oldRouterHealth := routerHealthFunc
	defer func() {
		detectWSLFunc = oldDetectWSL
		defaultAdminClientFunc = oldDefaultAdminClient
		routerHealthFunc = oldRouterHealth
	}()

	detectWSLFunc = func() bool { return false }
	defaultAdminClientFunc = func() (adminClient, error) {
		return nil, os.ErrNotExist
	}
	routerHealthFunc = func(context.Context) error {
		return nil
	}

	_, err := resolveRunRouter(context.Background(), io.Discard)
	if err == nil {
		t.Fatal("expected uncontrolled router error")
	}
	if !strings.Contains(err.Error(), "A gohere service is already using .localhost URLs") {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestResolveRunRouterHandlesTypedNilAdminClientAfterHealth(t *testing.T) {
	oldDetectWSL := detectWSLFunc
	oldDefaultAdminClient := defaultAdminClientFunc
	oldRouterHealth := routerHealthFunc
	defer func() {
		detectWSLFunc = oldDetectWSL
		defaultAdminClientFunc = oldDefaultAdminClient
		routerHealthFunc = oldRouterHealth
	}()

	detectWSLFunc = func() bool { return false }
	defaultAdminClientFunc = func() (adminClient, error) {
		var client *admin.Client
		return client, os.ErrNotExist
	}
	routerHealthFunc = func(context.Context) error {
		return nil
	}

	_, err := resolveRunRouter(context.Background(), io.Discard)
	if err == nil {
		t.Fatal("expected uncontrolled router error")
	}
	if !strings.Contains(err.Error(), "A gohere service is already using .localhost URLs") {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestLocalRouterControlErrorExplainsWindowsWSLRouterConflict(t *testing.T) {
	stateDir := t.TempDir()

	err := localRouterControlError("windows", stateDir)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "A WSL gohere service is using .localhost URLs") ||
		!strings.Contains(err.Error(), "gohere service stop") {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestLocalRouterControlErrorKeepsGenericMessageWhenWindowsStateExists(t *testing.T) {
	stateDir := t.TempDir()
	binDir := filepath.Join(stateDir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "gohere.exe"), []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "token"), []byte(strings.Repeat("a", 64)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	err := localRouterControlError("windows", stateDir)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "A gohere service is already using .localhost URLs") {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestResolveRunRouterStopsWhenWindowsRouterExistsButTokenNotFound(t *testing.T) {
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:         true,
		tokenErr:      bridge.ErrWindowsTokenNotFound,
		healthErr:     nil,
		windowsBinary: true,
	})
	defer restore()

	_, err := resolveRunRouter(context.Background(), io.Discard)
	if err == nil {
		t.Fatal("expected windows token error")
	}
	if !strings.Contains(err.Error(), "Windows gohere service is available, but WSL could not use it") {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestResolveRunRouterFallsBackWhenOnlyWSLLocalRouterLooksHealthy(t *testing.T) {
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:      true,
		tokenErr:   bridge.ErrWindowsTokenNotFound,
		healthErr:  nil,
		localAdmin: fakeAdminClient{},
	})
	defer restore()

	runRouter, err := resolveRunRouter(context.Background(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if runRouter.RouterLabel != "running" || runRouter.ChildHost != "127.0.0.1" || runRouter.RouteTargetHost != "127.0.0.1" {
		t.Fatalf("runRouter = %#v", runRouter)
	}
}

func TestResolveRunRouterStopsWhenWindowsRouterCannotReachWSL(t *testing.T) {
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:         true,
		token:         "windows-token",
		wslIP:         "172.20.10.2",
		reachable:     false,
		windowsBinary: true,
		admin:         &recordingAdminClient{},
	})
	defer restore()

	_, err := resolveRunRouter(context.Background(), io.Discard)
	if err == nil {
		t.Fatal("expected bridge reachability error")
	}
	if !strings.Contains(err.Error(), "Windows gohere service is running, but cannot reach WSL dev servers") ||
		!strings.Contains(err.Error(), "networkingMode=mirrored") ||
		!strings.Contains(err.Error(), "Windows Firewall") {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestResolveRunRouterIncludesBridgeProbeError(t *testing.T) {
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:         true,
		token:         "windows-token",
		wslIP:         "172.20.10.2",
		probeErr:      errors.New("probe endpoint failed"),
		windowsBinary: true,
		admin:         &recordingAdminClient{},
	})
	defer restore()

	_, err := resolveRunRouter(context.Background(), io.Discard)
	if err == nil {
		t.Fatal("expected bridge probe error")
	}
	if !strings.Contains(err.Error(), "probe endpoint failed") ||
		!strings.Contains(err.Error(), "Windows Firewall") {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestResolveRunRouterUsesLoopbackForwardingWhenWSLIPUnreachable(t *testing.T) {
	var probes []string
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:         true,
		token:         "windows-token",
		wslIP:         "172.20.10.2",
		windowsBinary: true,
		probeReachable: map[string]bool{
			"172.20.10.2": false,
			"127.0.0.1":   true,
		},
		probeHosts: &probes,
		admin:      &recordingAdminClient{},
	})
	defer restore()

	runRouter, err := resolveRunRouter(context.Background(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if runRouter.RouteTargetHost != "127.0.0.1" || runRouter.ChildHost != "0.0.0.0" || runRouter.RouterLabel != "Windows" {
		t.Fatalf("runRouter = %#v", runRouter)
	}
	wantProbes := []string{"127.0.0.1"}
	if !sameStrings(probes, wantProbes) {
		t.Fatalf("probes = %#v, want %#v", probes, wantProbes)
	}
}

func TestBridgeTargetCandidatesPreferLoopbackThenLocalhostThenWSLIP(t *testing.T) {
	got := bridgeTargetCandidates("172.20.10.2")
	want := []string{"127.0.0.1", "localhost", "172.20.10.2"}
	if !sameStrings(got, want) {
		t.Fatalf("bridgeTargetCandidates() = %#v, want %#v", got, want)
	}
}

func TestApplyRunRouterSetsStaticBridgeBindHost(t *testing.T) {
	plan := RunPlan{Static: true}

	applyRunRouter(&plan, runRouter{
		RouteTargetHost: "172.20.10.2",
		ChildHost:       "0.0.0.0",
		RouteSource:     "wsl",
		OwnerEnv:        "wsl",
		RouterLabel:     "Windows",
	})

	if plan.StaticBindHost != "0.0.0.0" {
		t.Fatalf("StaticBindHost = %q, want 0.0.0.0", plan.StaticBindHost)
	}
}

func TestApplyRunRouterPreservesExistingEnvWhenRebindingHost(t *testing.T) {
	plan := RunPlan{
		Port:    49231,
		Command: []string{"npm", "run", "dev", "--", "--host", "127.0.0.1"},
		Env:     []string{"PATH=/bin", "CUSTOM=kept", "HOST=127.0.0.1", "PORT=3000"},
	}

	applyRunRouter(&plan, runRouter{
		ChildHost: "0.0.0.0",
	})

	assertEnv(t, plan.Env, "CUSTOM", "kept")
	assertEnv(t, plan.Env, "HOST", "0.0.0.0")
	assertEnv(t, plan.Env, "PORT", "49231")
	if !strings.Contains(strings.Join(plan.Command, " "), "--host 0.0.0.0") {
		t.Fatalf("command = %#v, want bridge host replacement", plan.Command)
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

func TestListOutput(t *testing.T) {
	store := router.NewMemoryStore()
	store.Save([]router.Route{{
		Host:   "vibe-oke.localhost",
		Target: "http://127.0.0.1:46387",
		CWD:    "/tmp/vibe-oke",
		PID:    123,
	}})
	var out strings.Builder

	if err := ListWithStore(&out, store, false); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "host") || !strings.Contains(text, "target") || !strings.Contains(text, "status") {
		t.Fatalf("list output = %q", text)
	}
	if strings.Contains(text, "cwd") || strings.Contains(text, "pid") || strings.Contains(text, "backend") {
		t.Fatalf("normal list output is too noisy: %q", text)
	}
}

func TestListVerboseOutput(t *testing.T) {
	store := router.NewMemoryStore()
	store.Save([]router.Route{{
		Host:   "vibe-oke.localhost",
		Target: "http://127.0.0.1:46387",
		CWD:    "/tmp/vibe-oke",
		PID:    123,
	}})
	var out strings.Builder

	if err := ListWithStore(&out, store, true); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "host") || !strings.Contains(text, "target") || !strings.Contains(text, "status") || !strings.Contains(text, "pid") || !strings.Contains(text, "cwd") {
		t.Fatalf("verbose list output = %q", text)
	}
	if !strings.Contains(text, "vibe-oke.localhost") || !strings.Contains(text, "dead") || !strings.Contains(text, "123") || !strings.Contains(text, "/tmp/vibe-oke") {
		t.Fatalf("verbose list output = %q", text)
	}
	if strings.Contains(text, "backend") || strings.Contains(text, "cwd /tmp/vibe-oke") || strings.Contains(text, "pid 123") {
		t.Fatalf("verbose list output = %q", text)
	}
}

func TestListShowsUnknownWhenRouterIsNotReady(t *testing.T) {
	store := router.NewMemoryStore()
	store.Save([]router.Route{{
		Host:   "web.localhost",
		Target: "http://127.0.0.1:46563",
		CWD:    "/tmp/web",
		PID:    123,
	}})
	var out strings.Builder

	if err := ListWithStoreRouterReady(&out, store, false, false); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "web.localhost") || !strings.Contains(text, "unknown") {
		t.Fatalf("list output = %q", text)
	}
	if strings.Contains(text, "ready") || strings.Contains(text, "dead") {
		t.Fatalf("list output should not show target status while router is down: %q", text)
	}
}

func TestPruneOutput(t *testing.T) {
	tests := map[int]string{
		0: "No dead routes.\n",
		1: "Removed 1 dead route.\n",
		3: "Removed 3 dead routes.\n",
	}

	for removed, want := range tests {
		t.Run(want, func(t *testing.T) {
			var out strings.Builder
			if err := printPruneResult(&out, removed); err != nil {
				t.Fatal(err)
			}
			if out.String() != want {
				t.Fatalf("prune output = %q, want %q", out.String(), want)
			}
		})
	}
}

func TestStopOutput(t *testing.T) {
	var out strings.Builder

	printStopResult(&out, "vibe-oke.localhost", true, "")
	if out.String() != "Stopped vibe-oke.localhost.\n" {
		t.Fatalf("stop output = %q", out.String())
	}

	out.Reset()
	printStopResult(&out, "", false, "")
	if out.String() != "No running gohere project found for this folder.\n" {
		t.Fatalf("stop missing output = %q", out.String())
	}

	out.Reset()
	printStopResult(&out, "vibe-oke.localhost", false, "Could not verify the original gohere process. Not stopping PID 123.")
	if out.String() != "Could not verify the original gohere process. Not stopping PID 123.\n" {
		t.Fatalf("stop warning output = %q", out.String())
	}
}

func TestListUsesWindowsRouterFromWSL(t *testing.T) {
	admin := &recordingAdminClient{routes: []router.Route{{
		Host:   "web.localhost",
		Target: "http://172.20.10.2:5173",
		CWD:    "/home/roie/dev/web",
	}}}
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:         true,
		token:         "windows-token",
		windowsBinary: true,
		admin:         admin,
	})
	defer restore()

	var out strings.Builder
	if err := List(&out, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "web.localhost") {
		t.Fatalf("list output = %q", out.String())
	}
}

func TestPruneUsesWindowsRouterFromWSL(t *testing.T) {
	admin := &recordingAdminClient{routes: []router.Route{{
		Host:   "dead.localhost",
		Target: "http://127.0.0.1:1",
		PID:    999999,
		CWD:    "/home/roie/dev/dead",
	}}}
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:         true,
		token:         "windows-token",
		windowsBinary: true,
		admin:         admin,
	})
	defer restore()

	var out strings.Builder
	if err := Prune(&out); err != nil {
		t.Fatal(err)
	}
	if admin.deleted != "dead.localhost" {
		t.Fatalf("deleted = %q, want dead.localhost", admin.deleted)
	}
	if out.String() != "Removed 1 dead route.\n" {
		t.Fatalf("prune output = %q", out.String())
	}
}

func TestRouteManagerStopsWhenWindowsRouterExistsButTokenNotFound(t *testing.T) {
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:         true,
		tokenErr:      bridge.ErrWindowsTokenNotFound,
		windowsBinary: true,
	})
	defer restore()

	_, err := resolveRouteManager(context.Background())
	if err == nil {
		t.Fatal("expected windows token error")
	}
	if !strings.Contains(err.Error(), "Windows gohere service is available, but WSL could not use it") {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestRouteManagerFallsBackWhenOnlyWSLLocalRouterLooksHealthy(t *testing.T) {
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:      true,
		tokenErr:   bridge.ErrWindowsTokenNotFound,
		healthErr:  nil,
		localAdmin: fakeAdminClient{},
	})
	defer restore()

	manager, err := resolveRouteManager(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if manager.Client == nil || !manager.RouterReady {
		t.Fatalf("manager = %#v, want local ready router manager", manager)
	}
}

func TestRouteManagerStopsWhenWindowsRouterInstalledButNotRunning(t *testing.T) {
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:         true,
		token:         "windows-token",
		healthErr:     errors.New("connection refused"),
		windowsBinary: true,
	})
	defer restore()

	_, err := resolveRouteManager(context.Background())
	if err == nil {
		t.Fatal("expected windows router unavailable error")
	}
	if !strings.Contains(err.Error(), "Windows gohere is installed, but its service is not running") {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestRouteManagerFallsBackWhenOnlyWindowsTokenRemains(t *testing.T) {
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:      true,
		token:      "windows-token",
		healthErr:  errors.New("connection refused"),
		localAdmin: fakeAdminClient{},
	})
	defer restore()

	manager, err := resolveRouteManager(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if manager.Client == nil || !manager.RouterReady {
		t.Fatalf("manager = %#v, want local ready router manager", manager)
	}
}

func TestRouteManagerFallsBackWhenStaleWindowsTokenSeesWSLLocalRouter(t *testing.T) {
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:      true,
		token:      "windows-token",
		healthErr:  nil,
		localAdmin: fakeAdminClient{},
	})
	defer restore()

	manager, err := resolveRouteManager(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if manager.Client == nil || !manager.RouterReady {
		t.Fatalf("manager = %#v, want local ready router manager", manager)
	}
}

func TestStopUsesWindowsRouterFromWSL(t *testing.T) {
	cwd := "/home/roie/dev/web"
	admin := &recordingAdminClient{routes: []router.Route{{
		Host:     "web.localhost",
		Target:   "http://172.20.10.2:5173",
		CWD:      cwd,
		OwnerCWD: cwd,
		OwnerEnv: "wsl",
	}}}
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:         true,
		token:         "windows-token",
		windowsBinary: true,
		admin:         admin,
	})
	defer restore()

	var out strings.Builder
	if err := Stop(cwd, &out); err != nil {
		t.Fatal(err)
	}
	if admin.deleted != "web.localhost" {
		t.Fatalf("deleted = %q, want web.localhost", admin.deleted)
	}
	if out.String() != "Stopped web.localhost.\n" {
		t.Fatalf("stop output = %q", out.String())
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
	if !strings.Contains(out.String(), "ok service pid 12345") {
		t.Fatalf("doctor output = %q", out.String())
	}
}

func TestDoctorReportsWindowsRouterWhenWSLBridgeAvailable(t *testing.T) {
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:         true,
		token:         "windows-token",
		windowsBinary: true,
		admin:         &recordingAdminClient{},
	})
	defer restore()

	var out strings.Builder
	if err := DoctorWithChecks(&out, t.TempDir(), router.NewMemoryStore(), fakeAdminClient{}, DoctorChecks{
		Port80Available: func() bool { return true },
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ok environment WSL") ||
		!strings.Contains(out.String(), "ok windows service available") {
		t.Fatalf("doctor output = %q", out.String())
	}
}

func TestDoctorReportsMissingWindowsServiceInstallFromWSL(t *testing.T) {
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL: true,
	})
	defer restore()

	var out strings.Builder
	if err := DoctorWithChecks(&out, t.TempDir(), router.NewMemoryStore(), fakeAdminClient{}, DoctorChecks{
		Port80Available: func() bool { return true },
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ok environment WSL") ||
		!strings.Contains(out.String(), "fail windows service install missing") {
		t.Fatalf("doctor output = %q", out.String())
	}
}

func TestDoctorReportsWindowsServiceNotReachableFromWSL(t *testing.T) {
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:         true,
		windowsBinary: true,
		healthErr:     errors.New("connection refused"),
	})
	defer restore()

	var out strings.Builder
	if err := DoctorWithChecks(&out, t.TempDir(), router.NewMemoryStore(), fakeAdminClient{}, DoctorChecks{
		Port80Available: func() bool { return true },
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "fail windows service health unavailable") ||
		!strings.Contains(out.String(), "Run gohere from Windows first") {
		t.Fatalf("doctor output = %q", out.String())
	}
}

func TestDoctorReportsWindowsTokenMissingFromWSL(t *testing.T) {
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:         true,
		windowsBinary: true,
		tokenErr:      bridge.ErrWindowsTokenNotFound,
	})
	defer restore()

	var out strings.Builder
	if err := DoctorWithChecks(&out, t.TempDir(), router.NewMemoryStore(), fakeAdminClient{}, DoctorChecks{
		Port80Available: func() bool { return true },
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "fail windows service token missing") ||
		!strings.Contains(out.String(), "Run gohere from Windows first") {
		t.Fatalf("doctor output = %q", out.String())
	}
}

func TestDoctorReportsWindowsServiceAuthFailureFromWSL(t *testing.T) {
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:         true,
		token:         "windows-token",
		windowsBinary: true,
		admin:         unauthorizedBridgeAdminClient{},
	})
	defer restore()

	var out strings.Builder
	if err := DoctorWithChecks(&out, t.TempDir(), router.NewMemoryStore(), fakeAdminClient{}, DoctorChecks{
		Port80Available: func() bool { return true },
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "fail windows service auth failed") ||
		!strings.Contains(out.String(), "gohere service stop") {
		t.Fatalf("doctor output = %q", out.String())
	}
}

func TestDoctorReportsMissingWindowsInstallEvenWhenStaleTokenExists(t *testing.T) {
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:     true,
		token:     "windows-token",
		healthErr: nil,
		admin:     &recordingAdminClient{},
	})
	defer restore()

	var out strings.Builder
	if err := DoctorWithChecks(&out, t.TempDir(), router.NewMemoryStore(), fakeAdminClient{}, DoctorChecks{
		Port80Available: func() bool { return true },
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ok environment WSL") ||
		!strings.Contains(out.String(), "fail windows service install missing") {
		t.Fatalf("doctor output = %q", out.String())
	}
}

func TestDoctorDoesNotPanicWhenAdminClientCannotBeCreated(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
	}()

	defaultAdminClientFunc = func() (adminClient, error) {
		var client *admin.Client
		return client, errors.New("token unavailable")
	}
	var out strings.Builder

	if err := Doctor(&out); err != nil {
		t.Fatal(err)
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

func TestDoctorWithStoreShowsHintWhenPort80Blocked(t *testing.T) {
	stateDir := t.TempDir()
	store := router.NewMemoryStore()
	var out strings.Builder

	if err := DoctorWithChecks(&out, stateDir, store, nil, DoctorChecks{Port80Available: func() bool {
		return false
	}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "fail port 80 blocked\n  Try: stop the process using port 80, then run gohere again.\n") {
		t.Fatalf("doctor output = %q", out.String())
	}
}

func TestDoctorWithStoreShowsPermissionHintWhenPort80NeedsSetup(t *testing.T) {
	stateDir := t.TempDir()
	store := router.NewMemoryStore()
	var out strings.Builder

	if err := DoctorWithChecks(&out, stateDir, store, nil, DoctorChecks{Port80Status: func() Port80Status {
		return Port80Status{OK: false, Detail: "permission required", Hint: "Try: gohere setup"}
	}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "fail port 80 permission required\n  Try: gohere setup\n") {
		t.Fatalf("doctor output = %q", out.String())
	}
}

func TestDoctorWithStoreShowsInUseHintWhenPort80IsOwned(t *testing.T) {
	stateDir := t.TempDir()
	store := router.NewMemoryStore()
	var out strings.Builder

	if err := DoctorWithChecks(&out, stateDir, store, nil, DoctorChecks{Port80Status: func() Port80Status {
		return Port80Status{OK: false, Detail: "already in use", Hint: "Try: stop the process using port 80, then run gohere again."}
	}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "fail port 80 already in use\n  Try: stop the process using port 80, then run gohere again.\n") {
		t.Fatalf("doctor output = %q", out.String())
	}
}

func TestAddressInUseDetectionHandlesWindowsBindMessage(t *testing.T) {
	err := errors.New("listen tcp 127.0.0.1:80: bind: Only one usage of each socket address (protocol/network address/port) is normally permitted.")
	if !isAddressInUseError(err) {
		t.Fatal("expected Windows bind message to be classified as address in use")
	}
}

func TestDoctorWithStoreTreatsHealthyRouterAsPort80OK(t *testing.T) {
	var out strings.Builder

	if err := DoctorWithChecks(&out, t.TempDir(), router.NewMemoryStore(), fakeAdminClient{}, DoctorChecks{Port80Available: func() bool {
		return false
	}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ok port 80 used by gohere service") {
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

func TestDoctorWithStoreUsesWindowsStableBinary(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stateDir, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	binaryPath := filepath.Join(stateDir, "bin", "gohere.exe")
	if err := os.WriteFile(binaryPath, []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "token"), []byte("token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder

	if err := DoctorWithChecks(&out, stateDir, router.NewMemoryStore(), fakeAdminClient{}, DoctorChecks{
		GOOS: "windows",
	}); err != nil {
		t.Fatal(err)
	}
	output := out.String()
	if !strings.Contains(output, "ok stable binary "+binaryPath) {
		t.Fatalf("doctor output = %q", output)
	}
	if strings.Contains(output, "setcap") {
		t.Fatalf("doctor output should not include setcap on Windows: %q", output)
	}
	if strings.Contains(output, "token permissions") {
		t.Fatalf("doctor output should not include Unix token permissions on Windows: %q", output)
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

func (fakeAdminClient) ProbeTarget(context.Context, string) (bool, error) {
	return true, nil
}

type staleTokenAdminClient struct{}

func (staleTokenAdminClient) Health(context.Context) error {
	return nil
}

func (staleTokenAdminClient) Routes(context.Context) ([]router.Route, error) {
	return nil, admin.ErrUnauthorized
}

func (staleTokenAdminClient) UpsertRoute(context.Context, router.Route) error {
	return admin.ErrUnauthorized
}

func (staleTokenAdminClient) DeleteRoute(context.Context, string) error {
	return admin.ErrUnauthorized
}

type unauthorizedBridgeAdminClient struct {
	staleTokenAdminClient
}

func (unauthorizedBridgeAdminClient) ProbeTarget(context.Context, string) (bool, error) {
	return false, admin.ErrUnauthorized
}

type recordingAdminClient struct {
	mu             sync.Mutex
	upserted       chan struct{}
	upsertedClosed bool
	route          router.Route
	routes         []router.Route
	deleted        string
}

func (c *recordingAdminClient) Health(context.Context) error {
	return nil
}

func (c *recordingAdminClient) Routes(context.Context) ([]router.Route, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.routes, nil
}

func (c *recordingAdminClient) UpsertRoute(ctx context.Context, route router.Route) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.route = route
	if c.upserted == nil {
		c.upserted = make(chan struct{})
	}
	if !c.upsertedClosed {
		close(c.upserted)
		c.upsertedClosed = true
	}
	return nil
}

func (c *recordingAdminClient) DeleteRoute(ctx context.Context, host string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deleted = host
	return nil
}

func (c *recordingAdminClient) ProbeTarget(context.Context, string) (bool, error) {
	return true, nil
}

func (c *recordingAdminClient) waitForUpsert(t *testing.T) {
	t.Helper()
	c.mu.Lock()
	if c.upserted == nil {
		c.upserted = make(chan struct{})
	}
	upserted := c.upserted
	c.mu.Unlock()
	select {
	case <-upserted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for route registration")
	}
}

type bridgeStub struct {
	isWSL          bool
	healthErr      error
	token          string
	tokenErr       error
	windowsBinary  bool
	wslIP          string
	reachable      bool
	probeErr       error
	probeReachable map[string]bool
	probeHosts     *[]string
	admin          bridgeAdminClient
	localAdmin     adminClient
}

func stubBridgeDetection(t *testing.T, stub bridgeStub) func() {
	t.Helper()
	oldDetectWSL := detectWSLFunc
	oldWindowsHealth := windowsRouterHealthFunc
	oldDiscoverToken := discoverWindowsTokenFunc
	oldWindowsStableBinaryExists := windowsStableBinaryExists
	oldNewWindowsAdminClient := newWindowsAdminClientFunc
	oldCurrentWSLIP := currentWSLIPFunc
	oldProbeBridge := probeBridgeFunc
	oldDefaultAdminClient := defaultAdminClientFunc

	detectWSLFunc = func() bool {
		return stub.isWSL
	}
	windowsRouterHealthFunc = func(ctx context.Context) error {
		return stub.healthErr
	}
	discoverWindowsTokenFunc = func(string) (string, string, error) {
		if stub.tokenErr != nil {
			return "", "", stub.tokenErr
		}
		if stub.token == "" {
			return "", "", errors.New("no token")
		}
		return stub.token, "/mnt/c/Users/Jessa/.gohere/token", nil
	}
	windowsStableBinaryExists = func(string) bool {
		return stub.windowsBinary
	}
	newWindowsAdminClientFunc = func(string) bridgeAdminClient {
		if stub.admin == nil {
			return fakeAdminClient{}
		}
		return stub.admin
	}
	currentWSLIPFunc = func(context.Context) (string, error) {
		if stub.wslIP == "" {
			return "", errors.New("no wsl ip")
		}
		return stub.wslIP, nil
	}
	probeBridgeFunc = func(ctx context.Context, client bridgeProbeClient, wslIP string) (bool, string, error) {
		if stub.probeHosts != nil {
			*stub.probeHosts = append(*stub.probeHosts, wslIP)
		}
		if stub.probeErr != nil {
			return false, "http://" + wslIP + ":49231", stub.probeErr
		}
		if stub.probeReachable != nil {
			return stub.probeReachable[wslIP], "http://" + wslIP + ":49231", nil
		}
		return stub.reachable, "http://" + wslIP + ":49231", nil
	}
	defaultAdminClientFunc = func() (adminClient, error) {
		if stub.localAdmin == nil {
			return nil, errors.New("local admin not expected")
		}
		return stub.localAdmin, nil
	}

	return func() {
		detectWSLFunc = oldDetectWSL
		windowsRouterHealthFunc = oldWindowsHealth
		discoverWindowsTokenFunc = oldDiscoverToken
		windowsStableBinaryExists = oldWindowsStableBinaryExists
		newWindowsAdminClientFunc = oldNewWindowsAdminClient
		currentWSLIPFunc = oldCurrentWSLIP
		probeBridgeFunc = oldProbeBridge
		defaultAdminClientFunc = oldDefaultAdminClient
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
