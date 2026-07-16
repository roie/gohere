package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/roie/gohere/internal/admin"
	localcert "github.com/roie/gohere/internal/cert"
	"github.com/roie/gohere/internal/cli"
	"github.com/roie/gohere/internal/companion"
	appconfig "github.com/roie/gohere/internal/config"
	"github.com/roie/gohere/internal/lifecycle"
	"github.com/roie/gohere/internal/router"
	"github.com/roie/gohere/internal/runner"
	"github.com/roie/gohere/internal/setup"
)

type deferredTestGroupRunner struct {
	mu     sync.Mutex
	ctx    context.Context
	config runner.Config
	result *runner.Result
}

func (r *deferredTestGroupRunner) WaitReady() (*runner.Result, error) {
	result, err := startRunnerFunc(r.ctx, r.config)
	r.mu.Lock()
	r.result = result
	r.mu.Unlock()
	return result, err
}

func (r *deferredTestGroupRunner) Stop() {
	r.mu.Lock()
	result := r.result
	r.mu.Unlock()
	if result != nil {
		result.Stop()
	}
}

func TestMain(m *testing.M) {
	launchGroupRunnerFunc = func(ctx context.Context, cfg runner.Config) (groupRunner, error) {
		return &deferredTestGroupRunner{ctx: ctx, config: cfg}, nil
	}
	if runtime.GOOS == "windows" && isFakeWindowsNPMInvocation() {
		runFakeWindowsNPM()
		os.Exit(0)
	}
	detectWSLFunc = func() bool { return false }
	os.Exit(m.Run())
}

func isFakeWindowsNPMInvocation() bool {
	return os.Getenv("GOHERE_APP_FAKE_WINDOWS_NPM") == "1" && len(os.Args) > 1 && os.Args[1] == "run"
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
	if !plan.ManagedPort {
		t.Fatalf("ManagedPort = false, want true for injected script")
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
	if !plan.ManagedPort {
		t.Fatalf("ManagedPort = false, want true for injected script")
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
	if !plan.ManagedPort {
		t.Fatalf("ManagedPort = false, want true for --port-flag script")
	}
}

func TestPrepareRunDoesNotMarkUnknownScriptManaged(t *testing.T) {
	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"dev":"custom-dev"}}`,
	})

	plan, err := PrepareRun(cli.Command{Kind: cli.CommandRun, Script: "dev"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"npm", "run", "dev"}
	if !sameStrings(plan.Command, want) {
		t.Fatalf("command = %#v, want %#v", plan.Command, want)
	}
	if plan.ManagedPort {
		t.Fatalf("ManagedPort = true, want false when no port args were injected")
	}
}

func TestInjectedArgsControlPortRecognizesJoinedPortFlags(t *testing.T) {
	tests := [][]string{
		{"--", "--port=5173"},
		{"--", "-p5173"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if !injectedArgsControlPort(args, "") {
				t.Fatalf("injectedArgsControlPort(%#v) = false, want true", args)
			}
		})
	}
}

func TestRunPackageScriptWithWindowsLongPathSmoke(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows PATH smoke")
	}

	oldDefaultAdminClient := defaultAdminClientFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
	}()
	admin := &recordingAdminClient{}
	defaultAdminClientFunc = func() (adminClient, error) {
		return admin, nil
	}

	fakeNPMDir := t.TempDir()
	installFakeWindowsNPM(t, filepath.Join(fakeNPMDir, "npm.exe"))
	longPath := windowsLongPathEndingWith(t, fakeNPMDir, 8192)
	if len(longPath) <= 8191 {
		t.Fatalf("PATH length = %d, want > 8191", len(longPath))
	}
	t.Setenv("PATH", longPath)
	t.Setenv("GOHERE_APP_FAKE_WINDOWS_NPM", "1")

	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"dev":"custom-dev"}}`,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var stdout, stderr strings.Builder
	if err := Run(ctx, cli.Command{Kind: cli.CommandRun, Script: "dev"}, dir, &stdout, &stderr); err != nil {
		t.Fatalf("Run() error = %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	if admin.route.Host == "" || admin.route.Target == "" {
		t.Fatalf("route = %#v, want package script route registered\nstdout=%s\nstderr=%s", admin.route, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "gohere") || !strings.Contains(stdout.String(), ".localhost") {
		t.Fatalf("stdout = %q, want run success output", stdout.String())
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

func TestPrepareStaticProjectWithLive(t *testing.T) {
	dir := tempProject(t, map[string]string{"index.html": "<h1>Hello</h1>"})

	plan, err := PrepareRun(cli.Command{Kind: cli.CommandRun, Script: "dev", Live: true}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Static || !plan.Live {
		t.Fatalf("plan = %#v, want static live plan", plan)
	}
}

func TestPrepareRunRejectsLiveForPackageProject(t *testing.T) {
	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"dev":"vite"}}`,
	})

	_, err := PrepareRun(cli.Command{Kind: cli.CommandRun, Script: "dev", Live: true}, dir)
	if err == nil {
		t.Fatal("expected error")
	}
	want := "gohere error: --live is only supported for static files and folders."
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

func TestPrepareRawCommandRejectsLive(t *testing.T) {
	dir := tempProject(t, nil)

	_, err := PrepareRun(cli.Command{Kind: cli.CommandRaw, Raw: []string{"npm", "run", "dev"}, Live: true}, dir)
	if err == nil {
		t.Fatal("expected error")
	}
	want := "gohere error: --live is only supported for static files and folders."
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

func TestPrepareRunUsesAsAlias(t *testing.T) {
	tests := []struct {
		name     string
		cmd      cli.Command
		files    map[string]string
		wantHost string
		wantName string
		wantPath string
	}{
		{
			name:     "package project",
			cmd:      cli.Command{Kind: cli.CommandRun, Script: "dev", As: "api"},
			files:    map[string]string{"package.json": `{"scripts":{"dev":"vite"}}`},
			wantHost: "api.localhost",
			wantName: "api",
		},
		{
			name:     "script project",
			cmd:      cli.Command{Kind: cli.CommandRun, Script: "dev:web", As: "Web.API"},
			files:    map[string]string{"package.json": `{"scripts":{"dev:web":"vite"}}`},
			wantHost: "web-api.localhost",
			wantName: "web-api",
		},
		{
			name:     "raw command",
			cmd:      cli.Command{Kind: cli.CommandRaw, Raw: []string{"npm", "run", "dev"}, As: "api"},
			wantHost: "api.localhost",
			wantName: "api",
		},
		{
			name:     "static file",
			cmd:      cli.Command{Kind: cli.CommandRun, Script: "about.html", As: "docs"},
			files:    map[string]string{"about.html": "<h1>About</h1>"},
			wantHost: "docs.localhost",
			wantName: "docs",
			wantPath: "/about.html",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := tempProject(t, tt.files)
			plan, err := PrepareRun(tt.cmd, dir)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Host != tt.wantHost || plan.Name != tt.wantName || plan.URLPath != tt.wantPath {
				t.Fatalf("plan host/name/path = %q/%q/%q, want %q/%q/%q", plan.Host, plan.Name, plan.URLPath, tt.wantHost, tt.wantName, tt.wantPath)
			}
		})
	}
}

func TestPrepareRunRejectsEmptyAsAlias(t *testing.T) {
	dir := tempProject(t, map[string]string{"package.json": `{"scripts":{"dev":"vite"}}`})
	_, err := PrepareRun(cli.Command{Kind: cli.CommandRun, Script: "dev", As: "!!!"}, dir)
	if err == nil {
		t.Fatal("expected error")
	}
	want := "Invalid alias: !!!\nAliases can contain letters, numbers, spaces, dots, underscores, and hyphens."
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
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

func TestPrepareExplicitStaticDirectoryTarget(t *testing.T) {
	dir := tempProject(t, map[string]string{
		"package.json":      `{"scripts":{"dist":"vite"}}`,
		"dist/index.html":   "<h1>Build</h1>",
		"dist/about.html":   "<h1>About</h1>",
		"dist/assets.css":   "body{}",
		"public/index.html": "<h1>Public</h1>",
	})

	plan, err := PrepareRun(cli.Command{Kind: cli.CommandRun, TargetPath: "./dist", TargetPort: 5173}, dir)
	if err != nil {
		t.Fatal(err)
	}
	wantCWD := filepath.Join(dir, "dist")
	if !plan.Static || plan.CWD != wantCWD || plan.ProjectRoot != wantCWD {
		t.Fatalf("plan = %#v, want static cwd/project root %q", plan, wantCWD)
	}
	if plan.Host != "dist.localhost" || plan.Name != "dist" || plan.URLPath != "" {
		t.Fatalf("host/name/path = %q/%q/%q, want dist.localhost/dist/empty", plan.Host, plan.Name, plan.URLPath)
	}
}

func TestPrepareExplicitStaticDirectoryTargetWithLive(t *testing.T) {
	dir := tempProject(t, map[string]string{
		"package.json":    `{"scripts":{"dist":"vite"}}`,
		"dist/index.html": "<h1>Build</h1>",
	})

	plan, err := PrepareRun(cli.Command{Kind: cli.CommandRun, TargetPath: "./dist", Live: true, TargetPort: 5173}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Static || !plan.Live {
		t.Fatalf("plan = %#v, want static live path plan", plan)
	}
}

func TestPrepareStaticFileTargetWithLive(t *testing.T) {
	dir := tempProject(t, map[string]string{
		"index.html": "<h1>Hello</h1>",
		"about.html": "<h1>About</h1>",
	})

	plan, err := PrepareRun(cli.Command{Kind: cli.CommandRun, Script: "about.html", Live: true, TargetPort: 5173}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Static || !plan.Live || plan.URLPath != "/about.html" {
		t.Fatalf("plan = %#v, want static live file plan", plan)
	}
}

func TestPrepareExplicitPathTargetMissingDoesNotFallBackToScript(t *testing.T) {
	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"dist":"vite"}}`,
	})

	_, err := PrepareRun(cli.Command{Kind: cli.CommandRun, TargetPath: "./dist", TargetPort: 5173}, dir)
	if err == nil {
		t.Fatal("expected error")
	}
	want := "path not found: ./dist"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

func TestPrepareNonExplicitSlashArgumentRunsScript(t *testing.T) {
	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"apps/web":"vite"}}`,
	})

	plan, err := PrepareRun(cli.Command{Kind: cli.CommandRun, Script: "apps/web", ExplicitScript: true, TargetPort: 5173}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Static {
		t.Fatalf("plan = %#v, want package script", plan)
	}
	if command := strings.Join(plan.Command, " "); !strings.Contains(command, "npm run apps/web") {
		t.Fatalf("command = %q, want slash script", command)
	}
}

func TestPrepareExplicitPackageDirectoryTarget(t *testing.T) {
	root := tempProject(t, map[string]string{
		"package.json":          `{"name":"ctrltube","workspaces":["apps/*"]}`,
		"apps/web/package.json": `{"name":"@ctrltube/web","scripts":{"dev":"vite"}}`,
	})

	plan, err := PrepareRun(cli.Command{Kind: cli.CommandRun, TargetPath: "./apps/web", TargetPort: 5173}, root)
	if err != nil {
		t.Fatal(err)
	}
	wantCWD := filepath.Join(root, "apps", "web")
	if plan.Static || plan.CWD != wantCWD || plan.ProjectRoot != wantCWD {
		t.Fatalf("plan = %#v, want package cwd/project root %q", plan, wantCWD)
	}
	if plan.Host != "web.ctrltube.localhost" || plan.Name != "web.ctrltube" || plan.ProjectName != "web" {
		t.Fatalf("host/name/project = %q/%q/%q, want web.ctrltube.localhost/web.ctrltube/web", plan.Host, plan.Name, plan.ProjectName)
	}
	if command := strings.Join(plan.Command, " "); !strings.Contains(command, "npm run dev") {
		t.Fatalf("command = %q, want package dev command", command)
	}
}

func TestPrepareExistingNestedStaticFileTargetStillWorks(t *testing.T) {
	dir := tempProject(t, map[string]string{
		"index.html":            "<h1>Home</h1>",
		"apps/web/about.html":   "<h1>About</h1>",
		"apps/web/package.json": `{"scripts":{"dev":"vite"}}`,
	})

	plan, err := PrepareRun(cli.Command{Kind: cli.CommandRun, Script: "apps/web/about.html", TargetPort: 5173}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Static || plan.CWD != dir || plan.URLPath != "/apps/web/about.html" {
		t.Fatalf("plan = %#v, want static file from original cwd", plan)
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

func TestRunSuccessOutputLabelsExplicitPathTarget(t *testing.T) {
	got := runSuccessOutput(cli.Command{Kind: cli.CommandRun, TargetPath: "./dist"}, "dist.localhost", "")
	want := "gohere ./dist \u2192 http://dist.localhost\n"
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

func TestRunSuccessOutputCanUseHTTPS(t *testing.T) {
	got := runSuccessOutputForScheme(cli.Command{Kind: cli.CommandRun, Script: "dev"}, "https", "eventca.localhost", "")
	want := "gohere \u2192 https://eventca.localhost\n"
	if got != want {
		t.Fatalf("runSuccessOutputForScheme() = %q, want %q", got, want)
	}
}

func TestPublicURLSchemeHTTPFlagOverridesHTTPS(t *testing.T) {
	got := publicURLScheme(cli.Command{Kind: cli.CommandRun, Script: "dev", HTTP: true, URLScheme: "https"}, "https")
	if got != "http" {
		t.Fatalf("publicURLScheme() = %q, want http", got)
	}
}

func TestPublicURLSchemeUsesWindowsAuthorityHTTPS(t *testing.T) {
	got := publicURLScheme(cli.Command{Kind: cli.CommandRun, Script: "dev", URLScheme: AutoURLScheme}, "https")
	if got != "https" {
		t.Fatalf("publicURLScheme() = %q, want https", got)
	}
}

func TestCompanionPublicURLSchemeUsesHTTPSListener(t *testing.T) {
	info := companion.Info{Listeners: []companion.Listener{{Name: "http"}, {Name: "https"}}}
	if got := companionPublicURLScheme(info); got != "https" {
		t.Fatalf("companionPublicURLScheme() = %q, want https", got)
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
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("setup calls = %d, want 1", calls)
	}
	want := firstRunPrompt() + "\n"
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
	}, false)
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

func TestEnsureRouterPromptsAndStopsHealthyHTTPServiceWhenHTTPSRequired(t *testing.T) {
	oldSetup := setupFunc
	oldPromptInput := promptInput
	oldStartInstalledRouter := startInstalledRouterFunc
	oldServiceStop := serviceStopFunc
	defer func() {
		setupFunc = oldSetup
		promptInput = oldPromptInput
		startInstalledRouterFunc = oldStartInstalledRouter
		serviceStopFunc = oldServiceStop
	}()

	setupCalls := 0
	setupFunc = func(ctx context.Context) error {
		setupCalls++
		return nil
	}
	startInstalledRouterFunc = func(context.Context) error {
		t.Fatal("installed router restart should not run when current service is healthy")
		return nil
	}
	stopCalls := 0
	serviceStopFunc = func(context.Context) error {
		stopCalls++
		return nil
	}
	promptInput = strings.NewReader("\n")
	var out strings.Builder
	healthCalls := 0

	err := ensureRouter(context.Background(), &out, func(context.Context) error {
		healthCalls++
		return nil
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if stopCalls != 1 {
		t.Fatalf("service stop calls = %d, want 1", stopCalls)
	}
	if setupCalls != 1 {
		t.Fatalf("setup calls = %d, want 1", setupCalls)
	}
	if healthCalls < 2 {
		t.Fatalf("health calls = %d, want setup verification", healthCalls)
	}
	if out.String() != firstRunPrompt()+"\n" {
		t.Fatalf("prompt output = %q", out.String())
	}
}

func TestEnsureRouterPromptsWhenInstalledRouterStartsButStaysUnhealthy(t *testing.T) {
	oldSetup := setupFunc
	oldPromptInput := promptInput
	oldStartInstalledRouter := startInstalledRouterFunc
	defer func() {
		setupFunc = oldSetup
		promptInput = oldPromptInput
		startInstalledRouterFunc = oldStartInstalledRouter
	}()

	setupCalls := 0
	setupFunc = func(ctx context.Context) error {
		setupCalls++
		return nil
	}
	promptInput = strings.NewReader("\n")
	startInstalledCalls := 0
	startInstalledRouterFunc = func(context.Context) error {
		startInstalledCalls++
		return nil
	}
	healthCalls := 0
	var out strings.Builder

	err := ensureRouter(context.Background(), &out, func(context.Context) error {
		healthCalls++
		if setupCalls > 0 {
			return nil
		}
		return errors.New("router unavailable")
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if startInstalledCalls != 1 {
		t.Fatalf("installed router restart calls = %d, want 1", startInstalledCalls)
	}
	if setupCalls != 1 {
		t.Fatalf("setup calls = %d, want 1", setupCalls)
	}
	if healthCalls < 2 {
		t.Fatalf("health calls = %d, want restart attempt and setup verification", healthCalls)
	}
	if out.String() != firstRunPrompt()+"\n" {
		t.Fatalf("prompt output = %q", out.String())
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
	}, false)
	if err == nil {
		t.Fatal("expected decline error")
	}
	if err.Error() != "gohere was not enabled" {
		t.Fatalf("error = %q", err.Error())
	}
	want := firstRunPrompt() + "gohere was not enabled.\n\nRun gohere again when you are ready.\n"
	if out.String() != want {
		t.Fatalf("decline output = %q, want %q", out.String(), want)
	}
}

func TestEnsureRouterDoesNotRunSetupWhenPromptReadFailsEmpty(t *testing.T) {
	oldSetup := setupFunc
	oldPromptInput := promptInput
	oldStartInstalledRouter := startInstalledRouterFunc
	defer func() {
		setupFunc = oldSetup
		promptInput = oldPromptInput
		startInstalledRouterFunc = oldStartInstalledRouter
	}()

	setupFunc = func(ctx context.Context) error {
		t.Fatal("setup should not run when the setup prompt cannot be read")
		return nil
	}
	startInstalledRouterFunc = func(context.Context) error {
		return os.ErrNotExist
	}
	promptInput = failingPromptReader{}
	var out strings.Builder

	err := ensureRouter(context.Background(), &out, func(context.Context) error {
		return errors.New("router unavailable")
	}, false)
	if err == nil {
		t.Fatal("expected prompt read failure to decline setup")
	}
	if err.Error() != "gohere was not enabled" {
		t.Fatalf("error = %q", err.Error())
	}
	want := firstRunPrompt() + "gohere was not enabled.\n\nRun gohere again when you are ready.\n"
	if out.String() != want {
		t.Fatalf("prompt output = %q, want %q", out.String(), want)
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
	}, false)
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
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if healthCalls != 3 {
		t.Fatalf("health calls = %d, want 3", healthCalls)
	}
}

func TestEnsureRouterSetupFailureWrapsHealthError(t *testing.T) {
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
	healthErr := errors.New("router still starting")
	var out strings.Builder

	err := ensureRouter(context.Background(), &out, func(context.Context) error {
		return healthErr
	}, false)
	if !errors.Is(err, healthErr) {
		t.Fatalf("ensureRouter error = %v, want wrapped health error", err)
	}
	if !strings.Contains(err.Error(), "service is still not reachable") {
		t.Fatalf("ensureRouter error = %v", err)
	}
}

func TestEnsureHTTPSBrowserTrustRepairsMissingWindowsTrustFromWSL(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("WSL browser trust repair runs from Linux/WSL")
	}

	oldDetectWSL := detectWSLFunc
	oldWindowsTrust := windowsHTTPSCATrustedFunc
	oldRepairWindowsTrust := repairWindowsHTTPSTrustFunc
	defer func() {
		detectWSLFunc = oldDetectWSL
		windowsHTTPSCATrustedFunc = oldWindowsTrust
		repairWindowsHTTPSTrustFunc = oldRepairWindowsTrust
	}()
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDir := filepath.Join(home, ".gohere")
	if err := appconfig.Save(stateDir, appconfig.Config{HTTPS: true}); err != nil {
		t.Fatal(err)
	}
	store := localcert.Store{StateDir: stateDir}
	if _, err := store.EnsureCA(); err != nil {
		t.Fatal(err)
	}
	wantFingerprint, err := store.TrustFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	detectWSLFunc = func() bool { return true }
	trustCalls := 0
	windowsHTTPSCATrustedFunc = func(ctx context.Context, fingerprint string) (bool, string) {
		if fingerprint != wantFingerprint {
			t.Fatalf("fingerprint = %q, want %q", fingerprint, wantFingerprint)
		}
		trustCalls++
		return trustCalls > 1, "missing"
	}
	repairCalls := 0
	repairWindowsHTTPSTrustFunc = func(ctx context.Context, gotStateDir string) error {
		repairCalls++
		if gotStateDir != stateDir {
			t.Fatalf("state dir = %q, want %q", gotStateDir, stateDir)
		}
		return nil
	}

	if err := ensureHTTPSBrowserTrust(t.Context(), cli.Command{URLScheme: AutoURLScheme}); err != nil {
		t.Fatal(err)
	}
	if repairCalls != 1 {
		t.Fatalf("repair calls = %d, want 1", repairCalls)
	}
	if trustCalls != 2 {
		t.Fatalf("trust calls = %d, want pre and post repair checks", trustCalls)
	}
}

func TestEnsureHTTPSBrowserTrustSkipsRepairWhenWindowsTrustExists(t *testing.T) {
	oldDetectWSL := detectWSLFunc
	oldWindowsTrust := windowsHTTPSCATrustedFunc
	oldRepairWindowsTrust := repairWindowsHTTPSTrustFunc
	defer func() {
		detectWSLFunc = oldDetectWSL
		windowsHTTPSCATrustedFunc = oldWindowsTrust
		repairWindowsHTTPSTrustFunc = oldRepairWindowsTrust
	}()
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDir := filepath.Join(home, ".gohere")
	if err := appconfig.Save(stateDir, appconfig.Config{HTTPS: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := (localcert.Store{StateDir: stateDir}).EnsureCA(); err != nil {
		t.Fatal(err)
	}
	detectWSLFunc = func() bool { return true }
	windowsHTTPSCATrustedFunc = func(ctx context.Context, fingerprint string) (bool, string) {
		return true, "trusted"
	}
	repairWindowsHTTPSTrustFunc = func(ctx context.Context, gotStateDir string) error {
		t.Fatal("repair should not run when Windows trust exists")
		return nil
	}

	if err := ensureHTTPSBrowserTrust(t.Context(), cli.Command{URLScheme: AutoURLScheme}); err != nil {
		t.Fatal(err)
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

func TestSetupForGOOSUsesDarwinSetup(t *testing.T) {
	oldSetupDarwin := setupDarwinFunc
	defer func() {
		setupDarwinFunc = oldSetupDarwin
	}()
	calls := 0
	setupDarwinFunc = func(ctx context.Context, cfg setup.Config) error {
		calls++
		return nil
	}

	if err := setupForGOOS(context.Background(), "darwin"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("darwin setup calls = %d, want 1", calls)
	}
}

func TestSetupForGOOSUsesWSLTrustHookOnLinux(t *testing.T) {
	oldSetupLinux := setupLinuxFunc
	oldDetectWSL := detectWSLFunc
	defer func() {
		setupLinuxFunc = oldSetupLinux
		detectWSLFunc = oldDetectWSL
	}()
	detectWSLFunc = func() bool { return true }
	calls := 0
	setupLinuxFunc = func(ctx context.Context, cfg setup.Config) error {
		calls++
		if cfg.TrustCA == nil {
			t.Fatal("expected WSL setup to install a custom CA trust hook")
		}
		if cfg.Progress == nil {
			t.Fatal("expected setup progress writer")
		}
		return nil
	}

	if err := setupForGOOS(context.Background(), "linux"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("linux setup calls = %d, want 1", calls)
	}
}

func TestEnsureSingleActiveWSLOwnerRejectsActiveForeignOwner(t *testing.T) {
	route := router.Route{
		Host:            "foreign.localhost",
		OwnerEnv:        "wsl",
		OwnerInstance:   "owner-1",
		Distro:          "Debian",
		LinuxUser:       "bob",
		LeaseExpiresAt:  time.Now().Add(time.Minute),
		ProcessIdentity: "linux:123",
	}
	client := &recordingAdminClient{
		routes:   []router.Route{route},
		statuses: []router.RouteStatus{{Route: route, Status: "ready"}},
	}

	err := ensureSingleActiveWSLOwner(t.Context(), client, wslRunIdentity{OwnerInstance: "owner-2"})
	if err == nil || !strings.Contains(err.Error(), "another WSL owner (Debian/bob) is already using") {
		t.Fatalf("error = %v", err)
	}
	if client.deleted != "" {
		t.Fatalf("foreign route was deleted: %q", client.deleted)
	}
}

func TestEnsureSingleActiveWSLOwnerMarksExpiredDeadRoutePruneEligible(t *testing.T) {
	route := router.Route{
		Host:            "foreign.localhost",
		OwnerEnv:        "wsl",
		OwnerInstance:   "owner-1",
		Distro:          "Debian",
		LinuxUser:       "bob",
		LeaseExpiresAt:  time.Now().Add(-time.Minute),
		ProcessIdentity: "linux:123",
	}
	client := &recordingAdminClient{
		routes:   []router.Route{route},
		statuses: []router.RouteStatus{{Route: route, Status: "dead"}},
	}

	err := ensureSingleActiveWSLOwner(t.Context(), client, wslRunIdentity{OwnerInstance: "owner-2"})
	if err == nil || !strings.Contains(err.Error(), "foreign.localhost") || !strings.Contains(err.Error(), "gohere prune") {
		t.Fatalf("error = %v", err)
	}
	if client.deleted != "" {
		t.Fatalf("foreign route was deleted: %q", client.deleted)
	}
}

func TestEnsureSingleActiveWSLOwnerRefusesExpiredUncertainRoute(t *testing.T) {
	for _, status := range []string{"unknown", "ready"} {
		t.Run(status, func(t *testing.T) {
			route := router.Route{
				Host:            "foreign.localhost",
				OwnerEnv:        "wsl",
				OwnerInstance:   "owner-1",
				Distro:          "Debian",
				LinuxUser:       "bob",
				LeaseExpiresAt:  time.Now().Add(-time.Minute),
				ProcessIdentity: "linux:123",
			}
			client := &recordingAdminClient{
				routes:   []router.Route{route},
				statuses: []router.RouteStatus{{Route: route, Status: status}},
			}

			err := ensureSingleActiveWSLOwner(t.Context(), client, wslRunIdentity{OwnerInstance: "owner-2"})
			if err == nil || !strings.Contains(err.Error(), "refusing automatic ownership transfer") || !strings.Contains(err.Error(), status) {
				t.Fatalf("error = %v", err)
			}
			if client.deleted != "" {
				t.Fatalf("foreign route was deleted: %q", client.deleted)
			}
		})
	}
}

func TestRunStartsLocalProjectBeforeServiceRegistration(t *testing.T) {
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
		return &runner.Result{Port: cfg.ChosenPort}, nil
	}

	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"dev":"vite"}}`,
	})
	err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "dev"}, dir, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0] != "admin" || calls[1] != "runner" {
		t.Fatalf("calls = %#v, want reservation authority before runner", calls)
	}
}

func TestRunPlannedReservesExactEnvironmentBeforeStartupAndActivatesAfterReadiness(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() { defaultAdminClientFunc = oldDefaultAdminClient; startRunnerFunc = oldStartRunner }()
	t.Setenv("GOHERE_URL", "https://stale.localhost")
	t.Setenv("GOHERE_API_TARGET", "http://127.0.0.1:1")

	admin := &recordingAdminClient{}
	defaultAdminClientFunc = func() (adminClient, error) { return admin, nil }
	dir := tempProject(t, map[string]string{"package.json": `{"scripts":{"dev":"vite"}}`})
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		routes, err := admin.Routes(ctx)
		if err != nil || len(routes) != 1 || routes[0].EffectiveState() != router.RouteStatePending {
			t.Fatalf("routes before startup = %#v, err = %v", routes, err)
		}
		if admin.activateCalls != 0 {
			t.Fatal("route activated before child readiness")
		}
		wantURL := "http://" + routes[0].Host
		assertEnv(t, cfg.Env, "GOHERE_URL", wantURL)
		assertEnv(t, cfg.Env, "PORT", strconv.Itoa(cfg.ChosenPort))
		assertMissingEnv(t, cfg.Env, "GOHERE_API_TARGET")
		return &runner.Result{Port: cfg.ChosenPort}, nil
	}
	if err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "dev"}, dir, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if admin.reserveCalls != 1 || admin.activateCalls != 1 || admin.releaseCalls != 1 {
		t.Fatalf("lifecycle calls reserve/activate/release = %d/%d/%d", admin.reserveCalls, admin.activateCalls, admin.releaseCalls)
	}
}

func TestRunPlannedUsesCollisionSafeReservedHostnameInEnvironment(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() { defaultAdminClientFunc = oldDefaultAdminClient; startRunnerFunc = oldStartRunner }()
	dir := tempProject(t, map[string]string{"package.json": `{"scripts":{"dev":"vite"}}`})
	desiredHost := filepath.Base(dir) + ".localhost"
	admin := &recordingAdminClient{routes: []router.Route{{Host: desiredHost, Target: "http://127.0.0.1:49999", CWD: "/other/project"}}}
	defaultAdminClientFunc = func() (adminClient, error) { return admin, nil }
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		url := mustEnv(t, cfg.Env, "GOHERE_URL")
		if url == "http://"+desiredHost || !strings.HasSuffix(url, ".localhost") {
			t.Fatalf("collision-safe GOHERE_URL = %q, desired host was occupied", url)
		}
		return &runner.Result{Port: cfg.ChosenPort}, nil
	}
	if err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "dev"}, dir, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestPlannedReusableRouteRejectsWrongFixedTarget(t *testing.T) {
	dir := t.TempDir()
	route := router.Route{ID: "existing", Generation: 1, Host: "app.localhost", Target: "http://127.0.0.1:3002", CWD: dir}
	admin := &recordingAdminClient{routes: []router.Route{route}}
	plan := RunPlan{Host: route.Host, CWD: dir, Port: 3001, RouteTargetHost: "127.0.0.1", FixedPort: true}
	if got, reused, err := plannedReusableRoute(t.Context(), admin, plan); err != nil || reused {
		t.Fatalf("reuse = %#v/%v, err = %v; wrong fixed target must not be reused", got, reused, err)
	}
}

func TestVerifyPlannedWSLTargetRetriesReachability(t *testing.T) {
	oldTimeout, oldInterval := wslTargetProbeTimeout, wslTargetProbeInterval
	wslTargetProbeTimeout, wslTargetProbeInterval = 100*time.Millisecond, time.Millisecond
	defer func() { wslTargetProbeTimeout, wslTargetProbeInterval = oldTimeout, oldInterval }()
	admin := &recordingAdminClient{probeReachableAfter: 3}
	if err := verifyPlannedTarget(t.Context(), admin, "http://127.0.0.1:3001", true); err != nil {
		t.Fatal(err)
	}
	if admin.probeCalls != 3 {
		t.Fatalf("probe calls = %d, want retry until third probe", admin.probeCalls)
	}
}

func TestRunPlannedRollsBackReservationWhenActivationFails(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() { defaultAdminClientFunc = oldDefaultAdminClient; startRunnerFunc = oldStartRunner }()
	admin := &recordingAdminClient{activateErr: errors.New("activation failed")}
	defaultAdminClientFunc = func() (adminClient, error) { return admin, nil }
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		return &runner.Result{Port: cfg.ChosenPort}, nil
	}
	dir := tempProject(t, map[string]string{"package.json": `{"scripts":{"dev":"vite"}}`})
	err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "dev"}, dir, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "activation failed") {
		t.Fatalf("error = %v", err)
	}
	routes, _ := admin.Routes(context.Background())
	if len(routes) != 0 || admin.releaseCalls != 1 {
		t.Fatalf("reservation was not rolled back: routes=%#v release=%d", routes, admin.releaseCalls)
	}
}

func TestRunLazyTaskDoesNotResolveRouterOrInjectRuntimeURL(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() { defaultAdminClientFunc = oldDefaultAdminClient; startRunnerFunc = oldStartRunner }()
	defaultAdminClientFunc = func() (adminClient, error) { return nil, errors.New("router should not be touched") }
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		assertMissingEnv(t, cfg.Env, "GOHERE_URL")
		return nil, runner.ErrProcessFinished
	}
	dir := tempProject(t, map[string]string{"package.json": `{"scripts":{"build":"vite build"}}`})
	if err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "build", ExplicitScript: true}, dir, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestRunWSLReusesReadyRouteBeforeStartingRunner(t *testing.T) {
	oldStartRunner := startRunnerFunc
	defer func() {
		startRunnerFunc = oldStartRunner
	}()

	dir := tempProject(t, map[string]string{
		"package.json": `{"name":"ctrltube","scripts":{"dev":"vite"}}`,
	})
	admin := &recordingAdminClient{statuses: []router.RouteStatus{{
		Route: router.Route{
			ID:             "existing-route",
			Generation:     1,
			Host:           "ctrltube.localhost",
			Target:         "http://127.0.0.1:57940",
			CWD:            dir,
			ProjectRoot:    dir,
			ProjectName:    "ctrltube",
			OwnerEnv:       "wsl",
			OwnerInstance:  "test-owner",
			Distro:         "Ubuntu",
			LinuxUser:      "alice",
			RunnerID:       "existing-runner",
			LeaseExpiresAt: time.Now().Add(time.Minute),
		},
		Status: "ready",
	}}}
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:      true,
		wslIP:      "172.20.10.2",
		reachable:  true,
		admin:      admin,
		localAdmin: admin,
	})
	defer restore()

	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		t.Fatal("runner should not start when a ready route exists for the same WSL project")
		return nil, nil
	}

	var stdout, stderr strings.Builder
	if err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "dev"}, dir, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}

	want := "gohere \u2192 http://ctrltube.localhost\n"
	if stdout.String() != want || stderr.String() != "" {
		t.Fatalf("stdout=%q stderr=%q, want %q", stdout.String(), stderr.String(), want)
	}
	if admin.route.Host != "" {
		t.Fatalf("route was upserted = %#v, want existing route reused", admin.route)
	}
	if admin.deleted != "" {
		t.Fatalf("route was deleted = %q, want existing route kept", admin.deleted)
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
	if !strings.Contains(err.Error(), "router rejected its local control credential") {
		t.Fatalf("error = %q", err.Error())
	}
	if !strings.Contains(err.Error(), "gohere service stop") {
		t.Fatalf("error should recommend gohere service stop: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "gohere setup") {
		t.Fatalf("error should recommend setup in the same shell: %q", err.Error())
	}
	if strings.Contains(err.Error(), "systemctl") {
		t.Fatalf("error should not lead with systemd internals: %q", err.Error())
	}
	if strings.Contains(err.Error(), "GET /routes returned 401") {
		t.Fatalf("error leaked raw admin API response: %q", err.Error())
	}
}

func TestRunSuppressesChildStartupOutputOnSuccessfulStartup(t *testing.T) {
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
		return &runner.Result{Port: cfg.ChosenPort}, nil
	}

	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"dev":"vite"}}`,
	})
	var stdout, stderr strings.Builder
	err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "dev"}, dir, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	want := "gohere \u2192 http://" + filepath.Base(dir) + ".localhost\n"
	if stdout.String() != want {
		t.Fatalf("normal output = %q, want %q", stdout.String(), want)
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q", stderr.String())
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
		return &runner.Result{Port: cfg.ChosenPort}, nil
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

func TestRunUsesAsAliasInOutputAndRoute(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
	}()

	admin := &recordingAdminClient{}
	defaultAdminClientFunc = func() (adminClient, error) {
		return admin, nil
	}
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		return &runner.Result{Port: cfg.ChosenPort}, nil
	}

	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"dev:web":"vite"}}`,
	})
	var stdout, stderr strings.Builder
	err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "dev:web", As: "Web.API"}, dir, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if admin.route.Host != "web-api.localhost" || admin.route.Name != "web-api" {
		t.Fatalf("route host/name = %q/%q, want web-api.localhost/web-api", admin.route.Host, admin.route.Name)
	}
	want := "gohere dev:web \u2192 http://web-api.localhost\n"
	if stdout.String() != want || stderr.String() != "" {
		t.Fatalf("stdout=%q stderr=%q, want %q", stdout.String(), stderr.String(), want)
	}
}

func TestRegisterRouteCleanupLogsDeleteError(t *testing.T) {
	admin := &multiRecordingAdminClient{deleteErr: errors.New("delete failed")}
	plan := RunPlan{
		Host: "app.localhost",
		Name: "app",
		CWD:  t.TempDir(),
	}
	var stdout, stderr strings.Builder
	cleanup, err := registerRoute(context.Background(), admin, cli.Command{Kind: cli.CommandRun, Script: "dev"}, plan, 3000, os.Getpid(), &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}

	cleanup()

	if !strings.Contains(stderr.String(), "Could not remove route app.localhost") ||
		!strings.Contains(stderr.String(), "delete failed") {
		t.Fatalf("stderr = %q, want route cleanup warning", stderr.String())
	}
}

func TestRegisterRoutePersistsWSLNamespaceIdentityAndBothTargets(t *testing.T) {
	admin := &recordingAdminClient{}
	plan := RunPlan{
		Host:            "app.localhost",
		Name:            "app",
		CWD:             t.TempDir(),
		RouteTargetHost: "172.20.10.2",
		ListenHost:      "0.0.0.0",
		RouteSource:     "wsl",
		OwnerEnv:        "wsl",
		OwnerInstance:   "owner-1",
		Distro:          "Ubuntu",
		LinuxUser:       "alice",
		RunnerID:        "runner-1",
	}
	cleanup, err := registerRoute(t.Context(), admin, cli.Command{Kind: cli.CommandRun, Script: "dev"}, plan, 5173, os.Getpid(), io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	route := admin.route
	if route.Target != "http://172.20.10.2:5173" || route.ListenTarget != "http://0.0.0.0:5173" ||
		route.OwnerEnv != "wsl" || route.OwnerInstance != "owner-1" || route.Distro != "Ubuntu" ||
		route.LinuxUser != "alice" || route.RunnerID != "runner-1" || route.LeaseExpiresAt.IsZero() {
		t.Fatalf("route = %#v", route)
	}
}

func TestRegisterRouteStartsCleanupWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	admin := &recordingAdminClient{}
	plan := RunPlan{
		Host:          "app.localhost",
		CWD:           t.TempDir(),
		OwnerEnv:      "wsl",
		OwnerInstance: "owner-1",
		RunnerID:      "runner-1",
	}
	cleanup, err := registerRoute(ctx, admin, cli.Command{Kind: cli.CommandRun, Script: "dev"}, plan, 5173, os.Getpid(), io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	admin.mu.Lock()
	admin.routes = []router.Route{admin.route}
	admin.mu.Unlock()

	cancel()
	deadline := time.Now().Add(time.Second)
	for {
		admin.mu.Lock()
		deleted := admin.deleted
		admin.mu.Unlock()
		if deleted == "app.localhost" {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("deleted route = %q, want app.localhost", deleted)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRegisterRouteRefusesUnreachableWindowsToWSLTarget(t *testing.T) {
	oldTimeout := wslTargetProbeTimeout
	wslTargetProbeTimeout = 0
	defer func() { wslTargetProbeTimeout = oldTimeout }()
	target := "http://172.20.10.2:5173"
	admin := &recordingAdminClient{probeReachable: map[string]bool{target: false}}
	plan := RunPlan{
		Host:            "app.localhost",
		CWD:             t.TempDir(),
		RouteTargetHost: "172.20.10.2",
		OwnerEnv:        "wsl",
		OwnerInstance:   "owner-1",
		Distro:          "Ubuntu",
		LinuxUser:       "alice",
		RunnerID:        "runner-1",
	}

	_, err := registerRoute(t.Context(), admin, cli.Command{Kind: cli.CommandRun, Script: "dev"}, plan, 5173, os.Getpid(), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "Windows cannot reach WSL target") {
		t.Fatalf("error = %v", err)
	}
	if admin.route.Host != "" {
		t.Fatalf("unreachable route was registered: %#v", admin.route)
	}
	if admin.probeCalls != 1 {
		t.Fatalf("probe calls = %d, want 1", admin.probeCalls)
	}
}

func TestRegisterRouteWaitsForWindowsToReachStartingWSLTarget(t *testing.T) {
	oldTimeout := wslTargetProbeTimeout
	oldInterval := wslTargetProbeInterval
	wslTargetProbeTimeout = 100 * time.Millisecond
	wslTargetProbeInterval = time.Millisecond
	defer func() {
		wslTargetProbeTimeout = oldTimeout
		wslTargetProbeInterval = oldInterval
	}()

	admin := &recordingAdminClient{probeReachableAfter: 3}
	plan := RunPlan{
		Host:            "app.localhost",
		CWD:             t.TempDir(),
		RouteTargetHost: "127.0.0.1",
		OwnerEnv:        "wsl",
		OwnerInstance:   "owner-1",
		RunnerID:        "runner-1",
	}
	cleanup, err := registerRoute(t.Context(), admin, cli.Command{Kind: cli.CommandRun, Script: "dev"}, plan, 5173, os.Getpid(), io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if admin.probeCalls != 3 {
		t.Fatalf("probe calls = %d, want 3", admin.probeCalls)
	}
}

func TestWaitForWSLTargetStopsOnProbeError(t *testing.T) {
	want := errors.New("probe failed")
	admin := &recordingAdminClient{probeErr: want}
	reachable, err := waitForWSLTarget(t.Context(), admin, "http://127.0.0.1:5173")
	if reachable || !errors.Is(err, want) {
		t.Fatalf("reachable = %v, error = %v", reachable, err)
	}
	if admin.probeCalls != 1 {
		t.Fatalf("probe calls = %d, want 1", admin.probeCalls)
	}
}

func TestWaitForWSLTargetStopsWhenContextIsCanceled(t *testing.T) {
	oldInterval := wslTargetProbeInterval
	wslTargetProbeInterval = time.Hour
	defer func() { wslTargetProbeInterval = oldInterval }()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	admin := &recordingAdminClient{probeReachable: map[string]bool{}}
	reachable, err := waitForWSLTarget(ctx, admin, "http://127.0.0.1:5173")
	if reachable || !errors.Is(err, context.Canceled) {
		t.Fatalf("reachable = %v, error = %v", reachable, err)
	}
}

func TestRouteLeaseRenewsUntilRunnerCleanup(t *testing.T) {
	oldInterval := routeLeaseInterval
	oldDuration := routeLeaseDuration
	defer func() {
		routeLeaseInterval = oldInterval
		routeLeaseDuration = oldDuration
	}()
	routeLeaseInterval = 5 * time.Millisecond
	routeLeaseDuration = time.Minute
	client := &recordingAdminClient{}
	originalExpiry := time.Now().Add(time.Second)
	stop := startRouteLease(t.Context(), client, router.Route{
		Host:           "app.localhost",
		OwnerEnv:       "wsl",
		RunnerID:       "runner-1",
		LeaseExpiresAt: originalExpiry,
	}, io.Discard)
	client.waitForUpsert(t)
	stop()
	client.mu.Lock()
	renewed := client.route.LeaseExpiresAt
	client.mu.Unlock()
	if !renewed.After(originalExpiry) {
		t.Fatalf("renewed expiry = %s, want after %s", renewed, originalExpiry)
	}
}

func TestRouteCleanupDoesNotDeleteReplacementOwnedByAnotherRunner(t *testing.T) {
	client := &recordingAdminClient{routes: []router.Route{{
		Host:          "app.localhost",
		OwnerInstance: "owner-1",
		RunnerID:      "runner-new",
	}}}
	err := deleteOwnedRouteRegistration(t.Context(), client, router.Route{
		Host:          "app.localhost",
		OwnerInstance: "owner-1",
		RunnerID:      "runner-old",
	})
	if err != nil {
		t.Fatal(err)
	}
	if client.deleted != "" {
		t.Fatalf("deleted = %q, want replacement preserved", client.deleted)
	}
}

func TestRegisterRouteCleanupTimeoutUsesActionableWarning(t *testing.T) {
	admin := &multiRecordingAdminClient{deleteErr: context.DeadlineExceeded}
	plan := RunPlan{
		Host: "app.localhost",
		Name: "app",
		CWD:  t.TempDir(),
	}
	var stdout, stderr strings.Builder
	cleanup, err := registerRoute(context.Background(), admin, cli.Command{Kind: cli.CommandRun, Script: "dev"}, plan, 3000, os.Getpid(), &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}

	cleanup()

	want := "Could not remove route app.localhost before shutdown; run gohere prune if it still appears in gohere list.\n"
	if stderr.String() != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
}

func TestRegisterRouteCleanupSuppressesWarningWhenTimedOutDeleteRemovedRoute(t *testing.T) {
	admin := &cleanupVerifiedAdminClient{
		deleteErr: context.DeadlineExceeded,
		routesAfterDelete: []router.Route{{
			Host: "other.localhost",
		}},
	}
	plan := RunPlan{
		Host: "app.localhost",
		Name: "app",
		CWD:  t.TempDir(),
	}
	var stdout, stderr strings.Builder
	cleanup, err := registerRoute(context.Background(), admin, cli.Command{Kind: cli.CommandRun, Script: "dev"}, plan, 3000, os.Getpid(), &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}

	cleanup()

	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want no cleanup warning after route was removed", stderr.String())
	}
	if !admin.deleteCalled || !admin.routesChecked {
		t.Fatalf("deleteCalled=%v routesChecked=%v, want both true", admin.deleteCalled, admin.routesChecked)
	}
}

func TestRegisterRouteCleanupSuppressesWarningWhenTimedOutDeleteCompletesAfterCheck(t *testing.T) {
	admin := &cleanupVerifiedAdminClient{
		deleteErr: context.DeadlineExceeded,
		routesAfterDelete: []router.Route{{
			Host: "app.localhost",
		}},
		removeAfterRouteChecks: 2,
	}
	plan := RunPlan{
		Host: "app.localhost",
		Name: "app",
		CWD:  t.TempDir(),
	}
	var stdout, stderr strings.Builder
	cleanup, err := registerRoute(context.Background(), admin, cli.Command{Kind: cli.CommandRun, Script: "dev"}, plan, 3000, os.Getpid(), &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}

	cleanup()

	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want no cleanup warning after delayed route removal", stderr.String())
	}
	if admin.routeChecks < 2 {
		t.Fatalf("route checks = %d, want delayed follow-up checks", admin.routeChecks)
	}
}

func TestReplayWriterStreamsOnlyAfterStart(t *testing.T) {
	var out strings.Builder
	writer := newReplayWriter(1024, &out)

	writer.Write([]byte("startup hidden\n"))
	if out.String() != "" {
		t.Fatalf("output before start = %q, want empty", out.String())
	}

	writer.Start(false)
	writer.Write([]byte("future log\n"))
	if out.String() != "future log\n" {
		t.Fatalf("output after start = %q, want future log only", out.String())
	}
	if writer.capture().String() != "startup hidden\nfuture log\n" {
		t.Fatalf("capture = %q, want complete output for failure replay", writer.capture().String())
	}
}

func TestReplayWriterReplaysStartupWhenStartedVerbose(t *testing.T) {
	var out strings.Builder
	writer := newReplayWriter(1024, &out)

	writer.Write([]byte("startup shown\n"))
	writer.Start(true)
	writer.Write([]byte("future log\n"))

	want := "startup shown\nfuture log\n"
	if out.String() != want {
		t.Fatalf("output = %q, want %q", out.String(), want)
	}
}

func TestLinePrefixWriterPrefixesFutureLogs(t *testing.T) {
	var out strings.Builder
	writer := newLinePrefixWriter(&out, "[web] ")

	writer.Write([]byte("ready\nupdate"))
	writer.Write([]byte(" done\n"))

	want := "[web] ready\n[web] update done\n"
	if out.String() != want {
		t.Fatalf("prefixed output = %q, want %q", out.String(), want)
	}
}

func TestRunMultiScriptsRegistersRoutesAndOpensAllURLs(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	oldOpenBrowser := openBrowserFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
		openBrowserFunc = oldOpenBrowser
	}()

	admin := &multiRecordingAdminClient{}
	defaultAdminClientFunc = func() (adminClient, error) {
		return admin, nil
	}
	var commands []string
	var commandsMu sync.Mutex
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		commandsMu.Lock()
		commands = append(commands, strings.Join(cfg.Command, " "))
		commandsMu.Unlock()
		return &runner.Result{Port: cfg.ChosenPort}, nil
	}
	var opened []string
	openBrowserFunc = func(ctx context.Context, url string) error {
		opened = append(opened, url)
		return nil
	}

	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"dev:web":"vite","dev:api":"vite"}}`,
	})
	cmd := cli.Command{Kind: cli.CommandRun, Script: "dev:web", Scripts: []string{"dev:web", "dev:api"}, Open: true}
	var stdout, stderr strings.Builder
	if err := Run(context.Background(), cmd, dir, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}

	base := filepath.Base(dir)
	wantWeb := "web." + base + ".localhost"
	wantAPI := "api." + base + ".localhost"
	upsertedHosts := admin.upsertedHosts()
	if len(upsertedHosts) != 2 || !containsString(upsertedHosts, wantWeb) || !containsString(upsertedHosts, wantAPI) {
		t.Fatalf("upserted hosts = %#v, want %#v", upsertedHosts, []string{wantWeb, wantAPI})
	}
	if !sameStrings(opened, []string{"http://" + wantWeb, "http://" + wantAPI}) {
		t.Fatalf("opened = %#v", opened)
	}
	wantOut := fmt.Sprintf("gohere dev:web \u2192 http://%s\ngohere dev:api \u2192 http://%s\n", wantWeb, wantAPI)
	if stdout.String() != wantOut || stderr.String() != "" {
		t.Fatalf("stdout=%q stderr=%q, want %q", stdout.String(), stderr.String(), wantOut)
	}
	joinedCommands := strings.Join(commands, "\n")
	if len(commands) != 2 || !strings.Contains(joinedCommands, "dev:web") || !strings.Contains(joinedCommands, "dev:api") {
		t.Fatalf("commands = %#v", commands)
	}
}

func TestRunMultiReservesOneBatchAndStartsAllChildrenBeforeActivation(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() { defaultAdminClientFunc = oldDefaultAdminClient; startRunnerFunc = oldStartRunner }()

	admin := &multiRecordingAdminClient{}
	defaultAdminClientFunc = func() (adminClient, error) { return admin, nil }
	var mu sync.Mutex
	started := 0
	allStarted := make(chan struct{})
	envs := map[string][]string{}
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		command := strings.Join(cfg.Command, " ")
		label := "api"
		if strings.Contains(command, "dev:web") {
			label = "web"
		}
		mu.Lock()
		envs[label] = append([]string(nil), cfg.Env...)
		started++
		if started == 2 {
			close(allStarted)
		}
		mu.Unlock()
		select {
		case <-allStarted:
		case <-time.After(time.Second):
			return nil, errors.New("sibling was not started concurrently")
		}
		return &runner.Result{Port: cfg.ChosenPort}, nil
	}

	dir := tempProject(t, map[string]string{"package.json": `{"scripts":{"dev:web":"vite","dev:api":"vite"}}`})
	cmd := cli.Command{Kind: cli.CommandRun, Script: "dev:web", Scripts: []string{"dev:web", "dev:api"}}
	if err := Run(context.Background(), cmd, dir, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if admin.reserveCalls != 1 || admin.reservedBatch != 2 || admin.activationCalls != 1 || admin.releaseCalls != 1 {
		t.Fatalf("lifecycle reserve/batch/activate/release = %d/%d/%d/%d", admin.reserveCalls, admin.reservedBatch, admin.activationCalls, admin.releaseCalls)
	}
	webEnv, apiEnv := envValues(envs["web"]), envValues(envs["api"])
	base := filepath.Base(dir)
	if webEnv["GOHERE_URL"] != "http://web."+base+".localhost" || apiEnv["GOHERE_URL"] != "http://api."+base+".localhost" {
		t.Fatalf("self URLs web/api = %q/%q", webEnv["GOHERE_URL"], apiEnv["GOHERE_URL"])
	}
	for _, key := range []string{"GOHERE_WEB_URL", "GOHERE_WEB_TARGET", "GOHERE_WEB_PORT", "GOHERE_API_URL", "GOHERE_API_TARGET", "GOHERE_API_PORT"} {
		if webEnv[key] == "" || webEnv[key] != apiEnv[key] {
			t.Fatalf("named map %s differs: web=%q api=%q", key, webEnv[key], apiEnv[key])
		}
	}
}

func TestRunMultiActivationFailureReleasesNewBatchAndKeepsReusedRoute(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() { defaultAdminClientFunc = oldDefaultAdminClient; startRunnerFunc = oldStartRunner }()
	dir := tempProject(t, map[string]string{"package.json": `{"scripts":{"dev:web":"vite","dev:api":"vite"}}`})
	base := filepath.Base(dir)
	reused := router.Route{ID: "web-existing", Generation: 1, Host: "web." + base + ".localhost", Target: "http://127.0.0.1:49001", CWD: dir}
	admin := &multiRecordingAdminClient{routes: []router.Route{reused}, activateErr: errors.New("activation failed")}
	defaultAdminClientFunc = func() (adminClient, error) { return admin, nil }
	starts := 0
	var apiEnv []string
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		starts++
		apiEnv = append([]string(nil), cfg.Env...)
		if strings.Contains(strings.Join(cfg.Command, " "), "dev:web") {
			t.Fatal("reused web child started")
		}
		return &runner.Result{Port: cfg.ChosenPort}, nil
	}
	cmd := cli.Command{Kind: cli.CommandRun, Script: "dev:web", Scripts: []string{"dev:web", "dev:api"}}
	err := Run(context.Background(), cmd, dir, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "activation failed") {
		t.Fatalf("error = %v", err)
	}
	env := envValues(apiEnv)
	if env["GOHERE_WEB_TARGET"] != reused.Target || env["GOHERE_WEB_PORT"] != "49001" {
		t.Fatalf("reused discovery target/port = %q/%q, want %q/49001", env["GOHERE_WEB_TARGET"], env["GOHERE_WEB_PORT"], reused.Target)
	}
	if admin.probeCalls[reused.Target] < 2 {
		t.Fatalf("reused target probe calls = %d, want pre-reservation and pre-activation probes", admin.probeCalls[reused.Target])
	}
	routes, _ := admin.Routes(context.Background())
	if starts != 1 || len(routes) != 1 || routes[0].Ref() != reused.Ref() || admin.releaseCalls != 1 {
		t.Fatalf("starts=%d routes=%#v releases=%d; reused route must survive", starts, routes, admin.releaseCalls)
	}
}

func TestRunMultiScriptsSuppressesChildStartupOutput(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
	}()

	admin := &multiRecordingAdminClient{}
	defaultAdminClientFunc = func() (adminClient, error) {
		return admin, nil
	}
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		if strings.Contains(strings.Join(cfg.Command, " "), "dev:web") {
			cfg.Stdout.Write([]byte("web ready\n"))
		} else {
			cfg.Stderr.Write([]byte("api warning\n"))
		}
		return &runner.Result{Port: cfg.ChosenPort}, nil
	}

	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"dev:web":"vite","dev:api":"vite"}}`,
	})
	cmd := cli.Command{Kind: cli.CommandRun, Script: "dev:web", Scripts: []string{"dev:web", "dev:api"}}
	var stdout, stderr strings.Builder
	if err := Run(context.Background(), cmd, dir, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}

	base := filepath.Base(dir)
	wantOut := fmt.Sprintf("gohere dev:web \u2192 http://web.%s.localhost\ngohere dev:api \u2192 http://api.%s.localhost\n", base, base)
	if stdout.String() != wantOut {
		t.Fatalf("stdout=%q, want %q", stdout.String(), wantOut)
	}
	if stderr.String() != "" {
		t.Fatalf("stderr=%q, want no startup output", stderr.String())
	}
}

func TestRunMultiScriptsVerboseReplaysPrefixedStartupOutput(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
	}()

	admin := &multiRecordingAdminClient{}
	defaultAdminClientFunc = func() (adminClient, error) {
		return admin, nil
	}
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		if strings.Contains(strings.Join(cfg.Command, " "), "dev:web") {
			cfg.Stdout.Write([]byte("web ready\n"))
		} else {
			cfg.Stderr.Write([]byte("api warning\n"))
		}
		return &runner.Result{Port: cfg.ChosenPort}, nil
	}

	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"dev:web":"vite","dev:api":"vite"}}`,
	})
	cmd := cli.Command{Kind: cli.CommandRun, Script: "dev:web", Scripts: []string{"dev:web", "dev:api"}, Verbose: true}
	var stdout, stderr strings.Builder
	if err := Run(context.Background(), cmd, dir, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(stdout.String(), "[web] web ready\n") {
		t.Fatalf("stdout=%q, want prefixed web startup output", stdout.String())
	}
	if stderr.String() != "[api] api warning\n" {
		t.Fatalf("stderr=%q, want prefixed api startup output", stderr.String())
	}
}

func TestRunImplicitDevAtWorkspaceRootLaunchesWorkspacePackages(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
	}()

	admin := &multiRecordingAdminClient{}
	defaultAdminClientFunc = func() (adminClient, error) {
		return admin, nil
	}
	var commands []string
	var dirs []string
	var startsMu sync.Mutex
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		startsMu.Lock()
		commands = append(commands, strings.Join(cfg.Command, " "))
		dirs = append(dirs, cfg.Dir)
		startsMu.Unlock()
		return &runner.Result{Port: cfg.ChosenPort}, nil
	}

	root := tempProject(t, map[string]string{
		"package.json":             `{"name":"ctrltube","workspaces":["apps/*"],"scripts":{"dev":"pnpm --parallel --filter @ctrltube/worker --filter @ctrltube/web dev"}}`,
		"pnpm-lock.yaml":           "",
		"apps/web/package.json":    `{"name":"@ctrltube/web","scripts":{"dev":"vite"}}`,
		"apps/worker/package.json": `{"name":"@ctrltube/worker","scripts":{"dev":"wrangler dev"}}`,
	})

	var stdout, stderr strings.Builder
	if err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "dev"}, root, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}

	wantHosts := []string{"web.ctrltube.localhost", "worker.ctrltube.localhost"}
	if !sameStrings(admin.upsertedHosts(), wantHosts) {
		t.Fatalf("upserted hosts = %#v, want %#v", admin.upsertedHosts(), wantHosts)
	}
	wantDirs := []string{filepath.Join(root, "apps", "web"), filepath.Join(root, "apps", "worker")}
	if len(dirs) != 2 || !containsString(dirs, wantDirs[0]) || !containsString(dirs, wantDirs[1]) {
		t.Fatalf("runner dirs = %#v, want %#v", dirs, wantDirs)
	}
	if len(commands) != 2 || !strings.Contains(commands[0], "pnpm run dev") || !strings.Contains(commands[1], "pnpm run dev") {
		t.Fatalf("commands = %#v, want package dev commands", commands)
	}
	if strings.Contains(strings.Join(commands, "\n"), "--parallel --filter") {
		t.Fatalf("workspace mode should not run root aggregate script, commands = %#v", commands)
	}
	for _, route := range admin.upsertedRoutes() {
		if route.ProjectName != "ctrltube" || route.ProjectRoot != root {
			t.Fatalf("route project metadata = %q/%q, want ctrltube/%s", route.ProjectName, route.ProjectRoot, root)
		}
	}
	wantOut := "gohere web \u2192 http://web.ctrltube.localhost\n" +
		"gohere worker \u2192 http://worker.ctrltube.localhost\n"
	if stdout.String() != wantOut || stderr.String() != "" {
		t.Fatalf("stdout=%q stderr=%q, want %q", stdout.String(), stderr.String(), wantOut)
	}
}

func TestRunWorkspaceReusesReadyUnmanagedRouteFromSameCWD(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
	}()

	root := tempProject(t, map[string]string{
		"package.json":             `{"name":"ctrltube","workspaces":["apps/*"],"scripts":{"dev":"pnpm --parallel --filter @ctrltube/worker --filter @ctrltube/web dev"}}`,
		"pnpm-lock.yaml":           "",
		"apps/web/package.json":    `{"name":"@ctrltube/web","scripts":{"dev":"vite"}}`,
		"apps/worker/package.json": `{"name":"@ctrltube/worker","scripts":{"dev":"wrangler dev --local --port 8787"}}`,
	})
	workerDir := filepath.Join(root, "apps", "worker")
	admin := &multiRecordingAdminClient{routes: []router.Route{{
		ID:         "worker-existing",
		Generation: 1,
		Host:       "worker.ctrltube.localhost",
		Target:     "http://127.0.0.1:8787",
		CWD:        workerDir,
	}}}
	defaultAdminClientFunc = func() (adminClient, error) {
		return admin, nil
	}

	var dirs []string
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		dirs = append(dirs, filepath.Base(cfg.Dir))
		if filepath.Base(cfg.Dir) == "worker" {
			return nil, errors.New("address already in use")
		}
		return &runner.Result{Port: cfg.ChosenPort}, nil
	}

	var stdout, stderr strings.Builder
	if err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "dev"}, root, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}

	if !sameStrings(dirs, []string{"web"}) {
		t.Fatalf("started dirs = %#v, want only web", dirs)
	}
	routes, err := admin.Routes(context.Background())
	if err != nil || len(routes) != 1 || routes[0].Host != "worker.ctrltube.localhost" {
		t.Fatalf("routes after run = %#v, err=%v; reused worker must remain without duplication", routes, err)
	}
	wantOut := "gohere web \u2192 http://web.ctrltube.localhost\n" +
		"gohere worker \u2192 http://worker.ctrltube.localhost\n"
	if stdout.String() != wantOut || stderr.String() != "" {
		t.Fatalf("stdout=%q stderr=%q, want %q", stdout.String(), stderr.String(), wantOut)
	}
}

func TestRunWorkspaceReusesReadyManagedRouteFromSameCWD(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
	}()

	webServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer webServer.Close()

	root := tempProject(t, map[string]string{
		"package.json":          `{"name":"ctrltube","workspaces":["apps/*"],"scripts":{"dev":"pnpm --filter @ctrltube/web dev"}}`,
		"pnpm-lock.yaml":        "",
		"apps/web/package.json": `{"name":"@ctrltube/web","scripts":{"dev":"vite"}}`,
	})
	webDir := filepath.Join(root, "apps", "web")
	admin := &multiRecordingAdminClient{routes: []router.Route{{
		ID:         "web-existing",
		Generation: 1,
		Host:       "web.ctrltube.localhost",
		Target:     webServer.URL,
		CWD:        webDir,
	}}}
	defaultAdminClientFunc = func() (adminClient, error) {
		return admin, nil
	}

	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		t.Fatal("runner should not start when a ready managed route exists for the same workspace package")
		return nil, nil
	}

	var stdout, stderr strings.Builder
	if err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "dev"}, root, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}

	if countString(admin.upsertedHosts(), "web.ctrltube.localhost") != 1 {
		t.Fatalf("reused web route should not be duplicated: %#v", admin.upsertedHosts())
	}
	if deleted := admin.deletedHosts(); len(deleted) != 0 {
		t.Fatalf("deleted routes = %#v, want none", deleted)
	}
	wantOut := "gohere web \u2192 http://web.ctrltube.localhost\n"
	if stdout.String() != wantOut || stderr.String() != "" {
		t.Fatalf("stdout=%q stderr=%q, want %q", stdout.String(), stderr.String(), wantOut)
	}
}

func TestRunWorkspaceReusesUnknownManagedRouteFromSameCWD(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
	}()

	root := tempProject(t, map[string]string{
		"package.json":          `{"name":"ctrltube","workspaces":["apps/*"],"scripts":{"dev":"pnpm --filter @ctrltube/web dev"}}`,
		"pnpm-lock.yaml":        "",
		"apps/web/package.json": `{"name":"@ctrltube/web","scripts":{"dev":"vite"}}`,
	})
	webDir := filepath.Join(root, "apps", "web")
	route := router.Route{
		ID:         "web-unknown",
		Generation: 1,
		Host:       "web.ctrltube.localhost",
		Target:     "http://127.0.0.1:57940",
		CWD:        webDir,
	}
	admin := &recordingAdminClient{
		routes:   []router.Route{route},
		statuses: []router.RouteStatus{{Route: route, Status: "unknown"}},
	}
	defaultAdminClientFunc = func() (adminClient, error) {
		return admin, nil
	}

	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		t.Fatal("runner should not start when an unknown route exists for the same workspace package")
		return nil, nil
	}

	var stdout, stderr strings.Builder
	if err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "dev"}, root, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}

	wantOut := "gohere web \u2192 http://web.ctrltube.localhost\n"
	if stdout.String() != wantOut || stderr.String() != "" {
		t.Fatalf("stdout=%q stderr=%q, want %q", stdout.String(), stderr.String(), wantOut)
	}
	if admin.route.Host != "" {
		t.Fatalf("route was upserted = %#v, want existing unknown route reused", admin.route)
	}
	if admin.deleted != "" {
		t.Fatalf("route was deleted = %q, want existing route kept", admin.deleted)
	}
}

func TestRunWorkspaceInjectsServiceDiscoveryEnv(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
	}()

	defaultAdminClientFunc = func() (adminClient, error) {
		return &multiRecordingAdminClient{}, nil
	}
	envs := map[string][]string{}
	var envsMu sync.Mutex
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		envsMu.Lock()
		envs[filepath.Base(cfg.Dir)] = cfg.Env
		envsMu.Unlock()
		return &runner.Result{Port: cfg.ChosenPort}, nil
	}

	root := tempProject(t, map[string]string{
		"package.json":             `{"name":"ctrltube","workspaces":["apps/*"],"scripts":{"dev":"pnpm --parallel --filter @ctrltube/worker --filter @ctrltube/web dev"}}`,
		"pnpm-lock.yaml":           "",
		"apps/web/package.json":    `{"name":"@ctrltube/web","scripts":{"dev":"vite"}}`,
		"apps/worker/package.json": `{"name":"@ctrltube/worker","scripts":{"dev":"wrangler dev"}}`,
	})

	if err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "dev"}, root, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}

	if len(envs) != 2 {
		t.Fatalf("captured envs = %#v, want two workspace services", envs)
	}
	for name, env := range envs {
		assertEnv(t, env, "GOHERE_WEB_URL", "http://web.ctrltube.localhost")
		assertEnv(t, env, "GOHERE_WORKER_URL", "http://worker.ctrltube.localhost")
		webPort := mustEnv(t, env, "GOHERE_WEB_PORT")
		workerPort := mustEnv(t, env, "GOHERE_WORKER_PORT")
		assertEnv(t, env, "GOHERE_WEB_TARGET", "http://127.0.0.1:"+webPort)
		assertEnv(t, env, "GOHERE_WORKER_TARGET", "http://127.0.0.1:"+workerPort)

		assertMissingEnv(t, env, "GOHERE_SERVICES_JSON")
		if name == "" {
			t.Fatal("workspace service name is empty")
		}
	}
}

func TestRunWorkspaceServiceDiscoveryEnvUsesResolvedConflictHosts(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	oldChooseFreePortForHost := chooseFreePortForHostFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
		chooseFreePortForHostFunc = oldChooseFreePortForHost
	}()

	admin := &multiRecordingAdminClient{routes: []router.Route{{
		Host: "web.ctrltube.localhost",
		CWD:  "/other/web",
	}}}
	defaultAdminClientFunc = func() (adminClient, error) {
		return admin, nil
	}
	envs := map[string][]string{}
	var envsMu sync.Mutex
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		envsMu.Lock()
		envs[filepath.Base(cfg.Dir)] = cfg.Env
		envsMu.Unlock()
		return &runner.Result{Port: cfg.ChosenPort}, nil
	}
	nextPort := 5100
	chooseFreePortForHostFunc = func(host string) (int, error) {
		nextPort++
		return nextPort, nil
	}

	root := tempProject(t, map[string]string{
		"package.json":             `{"name":"ctrltube","workspaces":["apps/*"],"scripts":{"dev":"pnpm --parallel --filter @ctrltube/worker --filter @ctrltube/web dev"}}`,
		"pnpm-lock.yaml":           "",
		"apps/web/package.json":    `{"name":"@ctrltube/web","scripts":{"dev":"vite"}}`,
		"apps/worker/package.json": `{"name":"@ctrltube/worker","scripts":{"dev":"wrangler dev"}}`,
	})

	if err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "dev"}, root, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}

	const resolvedWebURL = "http://apps-web-ctrltube.localhost"
	if !containsString(admin.upsertedHosts(), "apps-web-ctrltube.localhost") {
		t.Fatalf("upserted hosts = %#v, want resolved conflict host", admin.upsertedHosts())
	}
	for name, env := range envs {
		assertEnv(t, env, "GOHERE_WEB_URL", resolvedWebURL)
		assertEnv(t, env, "GOHERE_WORKER_URL", "http://worker.ctrltube.localhost")

		assertMissingEnv(t, env, "GOHERE_SERVICES_JSON")
		if name == "" {
			t.Fatal("workspace service name is empty")
		}
	}
}

func TestRunMultiInjectsServiceDiscoveryEnv(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
	}()

	defaultAdminClientFunc = func() (adminClient, error) {
		return &multiRecordingAdminClient{}, nil
	}
	var envs [][]string
	var envsMu sync.Mutex
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		envsMu.Lock()
		envs = append(envs, cfg.Env)
		envsMu.Unlock()
		return &runner.Result{Port: cfg.ChosenPort}, nil
	}

	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"dev:web":"vite","dev:api":"wrangler dev"}}`,
	})
	cmd := cli.Command{Kind: cli.CommandRun, Script: "dev:web", Scripts: []string{"dev:web", "dev:api"}}
	if err := Run(context.Background(), cmd, dir, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}

	if len(envs) != 2 {
		t.Fatalf("captured %d envs, want two", len(envs))
	}
	base := filepath.Base(dir)
	for _, env := range envs {
		assertEnv(t, env, "GOHERE_WEB_URL", "http://web."+base+".localhost")
		assertEnv(t, env, "GOHERE_API_URL", "http://api."+base+".localhost")
		assertMissingEnv(t, env, "GOHERE_SERVICES_JSON")
	}
}

func TestRunServiceDiscoveryMarksExplicitPortScriptsUnmanaged(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
	}()

	defaultAdminClientFunc = func() (adminClient, error) {
		return &multiRecordingAdminClient{}, nil
	}
	var webEnv []string
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		if filepath.Base(cfg.Dir) == "web" {
			webEnv = cfg.Env
		}
		return &runner.Result{Port: cfg.ChosenPort}, nil
	}

	root := tempProject(t, map[string]string{
		"package.json":             `{"name":"ctrltube","workspaces":["apps/*"]}`,
		"pnpm-lock.yaml":           "",
		"apps/web/package.json":    `{"name":"@ctrltube/web","scripts":{"dev":"vite"}}`,
		"apps/worker/package.json": `{"name":"@ctrltube/worker","scripts":{"dev":"wrangler dev --local --port 8787"}}`,
	})

	if err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "dev"}, root, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}

	assertEnv(t, webEnv, "GOHERE_WORKER_URL", "http://worker.ctrltube.localhost")
	assertEnv(t, webEnv, "GOHERE_WORKER_PORT", "8787")
	assertEnv(t, webEnv, "GOHERE_WORKER_TARGET", "http://127.0.0.1:8787")
	assertMissingEnv(t, webEnv, "GOHERE_SERVICES_JSON")
}

func TestRunServiceDiscoveryMarksUnknownScriptsUnmanaged(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
	}()

	defaultAdminClientFunc = func() (adminClient, error) {
		return &multiRecordingAdminClient{}, nil
	}
	starts := 0
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		starts++
		return &runner.Result{Port: cfg.ChosenPort}, nil
	}

	root := tempProject(t, map[string]string{
		"package.json":             `{"name":"ctrltube","workspaces":["apps/*"]}`,
		"pnpm-lock.yaml":           "",
		"apps/web/package.json":    `{"name":"@ctrltube/web","scripts":{"dev":"vite"}}`,
		"apps/worker/package.json": `{"name":"@ctrltube/worker","scripts":{"dev":"custom-worker"}}`,
	})

	err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "dev"}, root, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "worker does not expose a controllable port") || !strings.Contains(err.Error(), "--port-flag") {
		t.Fatalf("error = %v, want unsupported adapter guidance", err)
	}
	if starts != 0 {
		t.Fatalf("started %d children before adapter validation", starts)
	}
}

func TestRunServiceDiscoveryPreservesExistingUserEnv(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
	}()
	t.Setenv("GOHERE_WORKER_URL", "http://custom.localhost")

	defaultAdminClientFunc = func() (adminClient, error) {
		return &multiRecordingAdminClient{}, nil
	}
	var webEnv []string
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		if filepath.Base(cfg.Dir) == "web" {
			webEnv = cfg.Env
		}
		return &runner.Result{Port: cfg.ChosenPort}, nil
	}

	root := tempProject(t, map[string]string{
		"package.json":             `{"name":"ctrltube","workspaces":["apps/*"]}`,
		"pnpm-lock.yaml":           "",
		"apps/web/package.json":    `{"name":"@ctrltube/web","scripts":{"dev":"vite"}}`,
		"apps/worker/package.json": `{"name":"@ctrltube/worker","scripts":{"dev":"wrangler dev"}}`,
	})

	if err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "dev"}, root, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}

	assertEnv(t, webEnv, "GOHERE_WORKER_URL", "http://worker.ctrltube.localhost")
	if mustEnv(t, webEnv, "GOHERE_WORKER_PORT") == "" {
		t.Fatal("expected generated worker port to remain available")
	}
}

func TestRunServiceDiscoveryEnvKeyCollisionFailsBeforeStarting(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
	}()

	defaultAdminClientFunc = func() (adminClient, error) {
		return &multiRecordingAdminClient{}, nil
	}
	starts := 0
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		starts++
		return &runner.Result{Port: cfg.ChosenPort}, nil
	}

	root := tempProject(t, map[string]string{
		"package.json":              `{"name":"ctrltube","workspaces":["apps/*"]}`,
		"pnpm-lock.yaml":            "",
		"apps/web-app/package.json": `{"name":"web-app","scripts":{"dev":"vite"}}`,
		"apps/web_app/package.json": `{"name":"web_app","scripts":{"dev":"vite"}}`,
	})

	err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "dev"}, root, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("expected env key collision error")
	}
	if !strings.Contains(err.Error(), `service env key "WEB_APP" is ambiguous`) {
		t.Fatalf("error = %q, want WEB_APP collision", err.Error())
	}
	if starts != 0 {
		t.Fatalf("started %d services before collision error, want zero", starts)
	}
}

func TestRunLiveAtWorkspaceRootDoesNotStartWorkspacePackages(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
	}()

	admin := &multiRecordingAdminClient{}
	defaultAdminClientFunc = func() (adminClient, error) {
		return admin, nil
	}
	var commands []string
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		commands = append(commands, strings.Join(cfg.Command, " "))
		return &runner.Result{Port: 5173}, nil
	}

	root := tempProject(t, map[string]string{
		"package.json":          `{"name":"ctrltube","workspaces":["apps/*"],"scripts":{"dev":"pnpm --parallel --filter @ctrltube/web dev"}}`,
		"pnpm-lock.yaml":        "",
		"apps/web/package.json": `{"name":"@ctrltube/web","scripts":{"dev":"vite"}}`,
	})

	var stdout, stderr strings.Builder
	err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "dev", Live: true}, root, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error")
	}
	want := "gohere error: --live is only supported for static files and folders."
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
	if len(commands) != 0 {
		t.Fatalf("workspace packages started with --live: %#v", commands)
	}
	if len(admin.upsertedHosts()) != 0 {
		t.Fatalf("upserted hosts = %#v, want none", admin.upsertedHosts())
	}
	if stdout.String() != "" || stderr.String() != "" {
		t.Fatalf("stdout=%q stderr=%q, want both empty", stdout.String(), stderr.String())
	}
}

func TestRunExplicitPathToWorkspaceRootBehavesLikeRunningThere(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
	}()

	admin := &multiRecordingAdminClient{}
	defaultAdminClientFunc = func() (adminClient, error) {
		return admin, nil
	}
	var commands []string
	var dirs []string
	var startsMu sync.Mutex
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		startsMu.Lock()
		commands = append(commands, strings.Join(cfg.Command, " "))
		dirs = append(dirs, cfg.Dir)
		startsMu.Unlock()
		return &runner.Result{Port: cfg.ChosenPort}, nil
	}

	parent := tempProject(t, map[string]string{
		"repo/package.json":             `{"name":"ctrltube","workspaces":["apps/*"],"scripts":{"dev":"pnpm --parallel --filter @ctrltube/web dev"}}`,
		"repo/pnpm-lock.yaml":           "",
		"repo/apps/web/package.json":    `{"name":"@ctrltube/web","scripts":{"dev":"vite"}}`,
		"repo/apps/worker/package.json": `{"name":"@ctrltube/worker","scripts":{"dev":"wrangler dev"}}`,
	})
	repo := filepath.Join(parent, "repo")

	var stdout, stderr strings.Builder
	cmd := cli.Command{Kind: cli.CommandRun, TargetPath: "./repo"}
	if err := Run(context.Background(), cmd, parent, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}

	wantHosts := []string{"web.ctrltube.localhost", "worker.ctrltube.localhost"}
	if !sameStrings(admin.upsertedHosts(), wantHosts) {
		t.Fatalf("upserted hosts = %#v, want %#v", admin.upsertedHosts(), wantHosts)
	}
	wantDirs := []string{filepath.Join(repo, "apps", "web"), filepath.Join(repo, "apps", "worker")}
	if len(dirs) != 2 || !containsString(dirs, wantDirs[0]) || !containsString(dirs, wantDirs[1]) {
		t.Fatalf("runner dirs = %#v, want %#v", dirs, wantDirs)
	}
	if len(commands) != 2 || !strings.Contains(commands[0], "pnpm run dev") || !strings.Contains(commands[1], "pnpm run dev") {
		t.Fatalf("commands = %#v, want workspace package dev commands", commands)
	}
	wantOut := "gohere web \u2192 http://web.ctrltube.localhost\n" +
		"gohere worker \u2192 http://worker.ctrltube.localhost\n"
	if stdout.String() != wantOut || stderr.String() != "" {
		t.Fatalf("stdout=%q stderr=%q, want %q", stdout.String(), stderr.String(), wantOut)
	}
}

func TestRunExplicitPackageDirectoryUsesTargetCWDAndLabelsPath(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
	}()

	admin := &multiRecordingAdminClient{}
	defaultAdminClientFunc = func() (adminClient, error) {
		return admin, nil
	}
	var dir string
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		dir = cfg.Dir
		return &runner.Result{Port: cfg.ChosenPort}, nil
	}

	root := tempProject(t, map[string]string{
		"package.json":          `{"name":"ctrltube","workspaces":["apps/*"]}`,
		"apps/web/package.json": `{"name":"@ctrltube/web","scripts":{"dev":"vite"}}`,
	})
	webDir := filepath.Join(root, "apps", "web")

	var stdout, stderr strings.Builder
	cmd := cli.Command{Kind: cli.CommandRun, TargetPath: "./apps/web"}
	if err := Run(context.Background(), cmd, root, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}

	if dir != webDir {
		t.Fatalf("runner dir = %q, want %q", dir, webDir)
	}
	if !sameStrings(admin.upsertedHosts(), []string{"web.ctrltube.localhost"}) {
		t.Fatalf("upserted hosts = %#v, want web.ctrltube.localhost", admin.upsertedHosts())
	}
	wantOut := "gohere ./apps/web \u2192 http://web.ctrltube.localhost\n"
	if stdout.String() != wantOut || stderr.String() != "" {
		t.Fatalf("stdout=%q stderr=%q, want %q", stdout.String(), stderr.String(), wantOut)
	}
}

func TestRunImplicitDevAtWorkspaceRootWithoutDevPackagesFallsBackToRootScript(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
	}()

	admin := &multiRecordingAdminClient{}
	defaultAdminClientFunc = func() (adminClient, error) {
		return admin, nil
	}
	var commands []string
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		commands = append(commands, strings.Join(cfg.Command, " "))
		return &runner.Result{Port: cfg.ChosenPort}, nil
	}

	root := tempProject(t, map[string]string{
		"package.json":          `{"name":"ctrltube","workspaces":["apps/*"],"scripts":{"dev":"pnpm --parallel --filter @ctrltube/web dev"}}`,
		"pnpm-lock.yaml":        "",
		"apps/web/package.json": `{"name":"@ctrltube/web","scripts":{"build":"vite build"}}`,
	})

	var stdout, stderr strings.Builder
	if err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "dev"}, root, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 || !strings.Contains(commands[0], "pnpm run dev") {
		t.Fatalf("commands = %#v, want root dev script", commands)
	}
	if !sameStrings(admin.upsertedHosts(), []string{"ctrltube.localhost"}) {
		t.Fatalf("upserted hosts = %#v, want ctrltube.localhost", admin.upsertedHosts())
	}
	wantOut := "gohere \u2192 http://ctrltube.localhost\n"
	if stdout.String() != wantOut || stderr.String() != "" {
		t.Fatalf("stdout=%q stderr=%q, want %q", stdout.String(), stderr.String(), wantOut)
	}
}

func TestRunExplicitScriptAtWorkspaceRootRunsRootScriptAsSingleRoute(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
	}()

	admin := &multiRecordingAdminClient{}
	defaultAdminClientFunc = func() (adminClient, error) {
		return admin, nil
	}
	var commands []string
	var dirs []string
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		commands = append(commands, strings.Join(cfg.Command, " "))
		dirs = append(dirs, cfg.Dir)
		return &runner.Result{Port: cfg.ChosenPort}, nil
	}

	root := tempProject(t, map[string]string{
		"package.json":             `{"name":"ctrltube","workspaces":["apps/*"],"scripts":{"preview":"pnpm --parallel --filter @ctrltube/worker --filter @ctrltube/web preview"}}`,
		"pnpm-lock.yaml":           "",
		"apps/web/package.json":    `{"name":"@ctrltube/web","scripts":{"preview":"vite preview"}}`,
		"apps/worker/package.json": `{"name":"@ctrltube/worker","scripts":{"preview":"wrangler dev --env preview"}}`,
	})

	var stdout, stderr strings.Builder
	cmd := cli.Command{Kind: cli.CommandRun, Script: "preview", ExplicitScript: true}
	if err := Run(context.Background(), cmd, root, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}

	if !sameStrings(admin.upsertedHosts(), []string{"ctrltube.localhost"}) {
		t.Fatalf("upserted hosts = %#v, want root route", admin.upsertedHosts())
	}
	if len(commands) != 1 || !strings.Contains(commands[0], "pnpm run preview") {
		t.Fatalf("commands = %#v, want one root preview command", commands)
	}
	if !sameStrings(dirs, []string{""}) {
		t.Fatalf("runner dirs = %#v, want empty root dir for existing behavior", dirs)
	}
	wantOut := "gohere preview \u2192 http://ctrltube.localhost\n"
	if stdout.String() != wantOut || stderr.String() != "" {
		t.Fatalf("stdout=%q stderr=%q, want %q", stdout.String(), stderr.String(), wantOut)
	}
}

func TestRunMultiAppliesRouterTargetToEveryRoute(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	oldResolveWSL := detectWSLFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
		detectWSLFunc = oldResolveWSL
	}()

	detectWSLFunc = func() bool { return false }
	admin := &multiRecordingAdminClient{}
	defaultAdminClientFunc = func() (adminClient, error) {
		return admin, nil
	}
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		return &runner.Result{Port: cfg.ChosenPort}, nil
	}

	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"dev:web":"vite","dev:api":"vite"}}`,
	})
	cmd := cli.Command{Kind: cli.CommandRun, Script: "dev:web", Scripts: []string{"dev:web", "dev:api"}}
	if err := Run(context.Background(), cmd, dir, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}

	for _, route := range admin.upsertedRoutes() {
		if !strings.HasPrefix(route.Target, "http://127.0.0.1:") {
			t.Fatalf("route target = %q, want router target host applied", route.Target)
		}
	}
}

func TestMultiScriptHostDerivesLabelFromScriptSuffix(t *testing.T) {
	tests := []struct {
		script string
		base   string
		want   string
	}{
		{script: "dev:web", base: "project.localhost", want: "web.project.localhost"},
		{script: "storybook", base: "project.localhost", want: "storybook.project.localhost"},
		{script: "dev:web.ui", base: "project.localhost", want: "web-ui.project.localhost"},
	}

	for _, tt := range tests {
		t.Run(tt.script, func(t *testing.T) {
			if got := multiScriptHost(tt.script, tt.base); got != tt.want {
				t.Fatalf("multiScriptHost(%q, %q) = %q, want %q", tt.script, tt.base, got, tt.want)
			}
		})
	}
}

func TestRunMultiScriptFinishedBeforeURLIsErrorAndCleansUp(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
	}()

	admin := &multiRecordingAdminClient{}
	defaultAdminClientFunc = func() (adminClient, error) {
		return admin, nil
	}
	calls := 0
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		calls++
		if calls == 2 {
			return nil, runner.ErrProcessFinished
		}
		return &runner.Result{Port: 5101}, nil
	}

	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"dev:api":"vite","lint":"eslint ."}}`,
	})
	cmd := cli.Command{Kind: cli.CommandRun, Script: "dev:api", Scripts: []string{"dev:api", "lint"}}
	var stdout, stderr strings.Builder
	err := Run(context.Background(), cmd, dir, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "lint does not expose a controllable port") || !strings.Contains(err.Error(), "--port-flag") {
		t.Fatalf("error = %q, want unsupported adapter guidance", err.Error())
	}
	if calls != 0 || len(admin.upsertedHosts()) != 0 {
		t.Fatalf("started=%d routes=%#v, want rejection before state/startup", calls, admin.upsertedHosts())
	}
	if stdout.String() != "" || stderr.String() != "" {
		t.Fatalf("stdout=%q stderr=%q, want no output", stdout.String(), stderr.String())
	}
}

func TestRunMultiReadinessFailureReleasesBatchWithoutActivation(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() { defaultAdminClientFunc = oldDefaultAdminClient; startRunnerFunc = oldStartRunner }()
	admin := &multiRecordingAdminClient{}
	defaultAdminClientFunc = func() (adminClient, error) { return admin, nil }
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		if strings.Contains(strings.Join(cfg.Command, " "), "dev:api") {
			_, _ = cfg.Stderr.Write([]byte("failed to bind api\n"))
			return nil, runner.ErrNoLocalURL
		}
		return &runner.Result{Port: cfg.ChosenPort}, nil
	}
	dir := tempProject(t, map[string]string{"package.json": `{"scripts":{"dev:web":"vite","dev:api":"vite"}}`})
	cmd := cli.Command{Kind: cli.CommandRun, Script: "dev:web", Scripts: []string{"dev:web", "dev:api"}}
	var stderr strings.Builder
	if err := Run(context.Background(), cmd, dir, io.Discard, &stderr); err == nil {
		t.Fatal("expected readiness failure")
	}
	if !strings.Contains(stderr.String(), "[api] failed to bind api\n") {
		t.Fatalf("stderr = %q, want prefixed startup failure output", stderr.String())
	}
	routes, _ := admin.Routes(context.Background())
	if admin.activationCalls != 0 || admin.releaseCalls != 1 || len(routes) != 0 {
		t.Fatalf("activate/release/routes = %d/%d/%#v, want 0/1/empty", admin.activationCalls, admin.releaseCalls, routes)
	}
}

func TestRunMultiStartedScriptFailureIncludesScriptName(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
	}()

	defaultAdminClientFunc = func() (adminClient, error) {
		return &multiRecordingAdminClient{}, nil
	}
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		helper := "print-port-sleep"
		if strings.Contains(strings.Join(cfg.Command, " "), "dev:api") {
			helper = "print-port-fail"
		}
		cfg.Command = appHelperCommand(helper)
		cfg.Env = append(cfg.Env, "GOHERE_APP_HELPER_PROCESS=1")
		cfg.StartupTimeout = time.Second
		return runner.Start(ctx, cfg)
	}

	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"dev:web":"vite","dev:api":"vite"}}`,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := cli.Command{Kind: cli.CommandRun, Script: "dev:web", Scripts: []string{"dev:web", "dev:api"}}
	var stdout, stderr strings.Builder
	err := Run(ctx, cmd, dir, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected failure")
	}
	if err.Error() != `gohere error: script "dev:api" failed.` {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestRunMultiRejectsStaticFileTarget(t *testing.T) {
	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"dev:web":"vite"}}`,
		"about.html":   "<h1>About</h1>",
	})
	cmd := cli.Command{Kind: cli.CommandRun, Script: "dev:web", Scripts: []string{"dev:web", "about.html"}}
	err := Run(context.Background(), cmd, dir, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("expected error")
	}
	want := "gohere error: multiple projects only support package scripts"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
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
		return &runner.Result{Port: cfg.ChosenPort}, nil
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
		return &runner.Result{Port: cfg.ChosenPort}, nil
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
	if admin.route.ID == "" || admin.route.Generation == 0 {
		t.Fatalf("static route identity is missing: %#v", admin.route)
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
	wantErr := "gohere error: started \"dev\", but no local URL was detected.\nTry:\n  gohere --target 5173 dev"
	if err.Error() != wantErr {
		t.Fatalf("error = %q, want %q", err.Error(), wantErr)
	}
}

func TestRunPrintsFinishedWhenScriptExitsBeforeURL(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
	}()

	defaultAdminClientFunc = func() (adminClient, error) {
		return nil, errors.New("service should not be touched")
	}
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		cfg.Stdout.Write([]byte("lint ok\n"))
		return nil, runner.ErrProcessFinished
	}

	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"lint":"eslint ."}}`,
	})
	var stdout, stderr strings.Builder
	err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "lint"}, dir, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "gohere lint finished.\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if stderr.String() != "lint ok\n" {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunFormatsNoURLTimeoutWithScriptName(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
	}()

	defaultAdminClientFunc = func() (adminClient, error) {
		return nil, errors.New("service should not be touched")
	}
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		return nil, runner.ErrNoLocalURL
	}

	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"worker":"node worker.js"}}`,
	})
	var stdout, stderr strings.Builder
	err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "worker"}, dir, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected startup error")
	}
	want := "gohere error: started \"worker\", but no local URL was detected.\nTry:\n  gohere --target 5173 worker"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

func TestRunFormatsFailedScriptExit(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
	}()

	defaultAdminClientFunc = func() (adminClient, error) {
		return nil, errors.New("service should not be touched")
	}
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		cfg.Stderr.Write([]byte("lint failed\n"))
		return nil, runner.ErrProcessFailed
	}

	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"lint":"eslint ."}}`,
	})
	var stdout, stderr strings.Builder
	err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "lint"}, dir, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected failure")
	}
	if err.Error() != "gohere error: script \"lint\" failed." {
		t.Fatalf("error = %q", err.Error())
	}
	if stderr.String() != "lint failed\n" {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunFailureReplayAddsTrailingNewline(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	oldStartRunner := startRunnerFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		startRunnerFunc = oldStartRunner
	}()

	defaultAdminClientFunc = func() (adminClient, error) {
		return nil, errors.New("service should not be touched")
	}
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		cfg.Stderr.Write([]byte("lint failed"))
		return nil, runner.ErrProcessFailed
	}

	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"lint":"eslint ."}}`,
	})
	var stdout, stderr strings.Builder
	err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "lint"}, dir, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected failure")
	}
	if stderr.String() != "lint failed\n" {
		t.Fatalf("stderr = %q, want trailing newline", stderr.String())
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
	chosenPort := 0
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		chosenPort = cfg.ChosenPort
		cfg.Stdout.Write([]byte("Local: http://127.0.0.1:5173/\n"))
		return &runner.Result{Port: cfg.ChosenPort}, nil
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
		!strings.Contains(stdout.String(), fmt.Sprintf("\ntarget: http://127.0.0.1:%d\n", chosenPort)) ||
		!strings.Contains(stdout.String(), "project root: "+dir+"\n") ||
		!strings.Contains(stdout.String(), "command: npm run dev -- --host 127.0.0.1 --port ") ||
		!strings.Contains(stdout.String(), "service: running\n") ||
		!strings.Contains(stdout.String(), "Local: http://127.0.0.1:5173/\n") {
		t.Fatalf("verbose stdout = %q", stdout.String())
	}
}

func TestWithHostRewritesCommonLoopbackHosts(t *testing.T) {
	command := []string{"vite", "--host", "localhost", "--allowed-hosts", "127.0.0.1", "--allowed-hosts=localhost", "--listen", "0.0.0.0", "--hostname=localhost"}
	got := withHost(command, "192.0.2.10")
	want := []string{"vite", "--host", "192.0.2.10", "--allowed-hosts", "192.0.2.10", "--allowed-hosts=192.0.2.10", "--listen", "192.0.2.10", "--hostname=192.0.2.10"}
	if !sameStrings(got, want) {
		t.Fatalf("withHost() = %#v, want %#v", got, want)
	}
}

func TestWithHostDoesNotRewritePositionalLoopbackHost(t *testing.T) {
	command := []string{"custom-dev", "localhost", "--host", "127.0.0.1"}
	got := withHost(command, "192.0.2.10")
	want := []string{"custom-dev", "localhost", "--host", "192.0.2.10"}
	if !sameStrings(got, want) {
		t.Fatalf("withHost() = %#v, want %#v", got, want)
	}
}

func TestFirstRunPromptMentionsSudoOnLinux(t *testing.T) {
	if !strings.Contains(firstRunPromptForGOOS("linux"), "sudo") {
		t.Fatalf("linux prompt should mention sudo: %q", firstRunPromptForGOOS("linux"))
	}
	if !strings.Contains(firstRunPromptForGOOS("linux"), "certificate authority") {
		t.Fatalf("prompt should mention certificate authority: %q", firstRunPromptForGOOS("linux"))
	}
	if !strings.Contains(firstRunPromptForGOOS("windows"), "HTTPS .localhost") {
		t.Fatalf("prompt should mention HTTPS .localhost: %q", firstRunPromptForGOOS("windows"))
	}
	if strings.Contains(firstRunPromptForGOOS("windows"), "sudo") {
		t.Fatalf("windows prompt should not mention sudo: %q", firstRunPromptForGOOS("windows"))
	}
	if strings.Contains(firstRunPromptForGOOS("darwin"), "sudo") {
		t.Fatalf("darwin prompt should not mention sudo: %q", firstRunPromptForGOOS("darwin"))
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
		return &runner.Result{Port: cfg.ChosenPort}, nil
	}

	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"dev":"vite"}}`,
	})
	var stdout strings.Builder
	if err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "dev", Verbose: true}, dir, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}

	assertEnv(t, gotEnv, "HOST", "127.0.0.1")
	if strings.Contains(strings.Join(gotCommand, " "), "--host 0.0.0.0") {
		t.Fatalf("command = %#v, should not bind all interfaces when Windows can reach WSL loopback", gotCommand)
	}
	port := mustEnv(t, gotEnv, "PORT")
	if !strings.Contains(stdout.String(), "\ntarget: http://127.0.0.1:"+port+"\n") ||
		!strings.Contains(stdout.String(), "service: Windows\n") {
		t.Fatalf("verbose stdout = %q", stdout.String())
	}
}

func TestResolveRunRouterDoesNotFallBackWhenWindowsRouterIsUnavailable(t *testing.T) {
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:      true,
		healthErr:  errors.New("connection refused"),
		localAdmin: fakeAdminClient{},
	})
	defer restore()

	_, err := resolveRunRouter(context.Background(), io.Discard, cli.Command{})
	if err == nil || !strings.Contains(err.Error(), "No WSL router was started") ||
		!strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("error = %v", err)
	}
}

func TestResolveRunRouterReportsWindowsStartFailureWhenInstalled(t *testing.T) {
	startCalls := 0
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:             true,
		token:             "windows-token",
		healthErr:         errors.New("connection refused"),
		windowsBinary:     true,
		startWindowsErr:   errors.New("start failed"),
		startWindowsCalls: &startCalls,
	})
	defer restore()

	_, err := resolveRunRouter(context.Background(), io.Discard, cli.Command{})
	if err == nil {
		t.Fatal("expected Windows start failure")
	}
	if startCalls != 1 {
		t.Fatalf("start calls = %d, want 1", startCalls)
	}
	if !strings.Contains(err.Error(), "could not use the Windows authority from WSL") ||
		!strings.Contains(err.Error(), "No WSL router was started") ||
		!strings.Contains(err.Error(), "start failed") {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestResolveRunRouterStartsWindowsServiceFromWSLWhenInstalledButStopped(t *testing.T) {
	startCalls := 0
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:                    true,
		token:                    "windows-token",
		healthErr:                errors.New("connection refused"),
		windowsBinary:            true,
		startWindowsCalls:        &startCalls,
		startWindowsMakesHealthy: true,
		wslIP:                    "172.20.10.2",
		reachable:                true,
	})
	defer restore()

	runRouter, err := resolveRunRouter(context.Background(), io.Discard, cli.Command{})
	if err != nil {
		t.Fatal(err)
	}
	if startCalls != 1 {
		t.Fatalf("start calls = %d, want 1", startCalls)
	}
	if runRouter.RouterLabel != "Windows" || !runRouter.Bridge {
		t.Fatalf("runRouter = %#v", runRouter)
	}
}

func TestResolveRunRouterDoesNotUseLocalRouterWhenCompanionHealthFails(t *testing.T) {
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:      true,
		token:      "windows-token",
		healthErr:  errors.New("connection refused"),
		localAdmin: fakeAdminClient{},
	})
	defer restore()

	_, err := resolveRunRouter(context.Background(), io.Discard, cli.Command{})
	if err == nil || !strings.Contains(err.Error(), "No WSL router was started") {
		t.Fatalf("error = %v", err)
	}
}

func TestResolveRunRouterIgnoresStaleTokenAndLocalRouterWhenCompanionWorks(t *testing.T) {
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:      true,
		token:      "windows-token",
		healthErr:  nil,
		wslIP:      "172.20.10.2",
		reachable:  true,
		localAdmin: fakeAdminClient{},
	})
	defer restore()

	runRouter, err := resolveRunRouter(context.Background(), io.Discard, cli.Command{})
	if err != nil {
		t.Fatal(err)
	}
	if runRouter.RouterLabel != "Windows" || !runRouter.Bridge {
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

	runRouter, err := resolveRunRouter(context.Background(), io.Discard, cli.Command{})
	if err != nil {
		t.Fatal(err)
	}
	if runRouter.Client == nil || runRouter.RouteTargetHost != "127.0.0.1" {
		t.Fatalf("runRouter = %#v", runRouter)
	}
}

func TestResolveRunRouterReportsUncontrolledRouterWhenTokenMissingAfterHealth(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test exercises the non-Windows local-router diagnostic")
	}
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

	_, err := resolveRunRouter(context.Background(), io.Discard, cli.Command{})
	if err == nil {
		t.Fatal("expected uncontrolled router error")
	}
	if !strings.Contains(err.Error(), "gohere service") {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestResolveRunRouterHandlesTypedNilAdminClientAfterHealth(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test exercises the non-Windows local-router diagnostic")
	}
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

	_, err := resolveRunRouter(context.Background(), io.Discard, cli.Command{})
	if err == nil {
		t.Fatal("expected uncontrolled router error")
	}
	if !strings.Contains(err.Error(), "gohere service") {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestLocalRouterControlErrorRejectsNonWindowsPortOwner(t *testing.T) {
	stateDir := t.TempDir()

	err := localRouterControlError("windows", stateDir)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "A non-Windows process is occupying gohere's .localhost service ports") ||
		!strings.Contains(err.Error(), "gohere doctor") {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestLocalRouterControlErrorReportsStaleCredentialWhenWindowsStateExists(t *testing.T) {
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
	if !strings.Contains(err.Error(), "router rejected its local control credential") ||
		!strings.Contains(err.Error(), "gohere setup") {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestResolveRunRouterDoesNotReadWindowsTokenWhenCompanionExists(t *testing.T) {
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:         true,
		healthErr:     nil,
		windowsBinary: true,
		wslIP:         "172.20.10.2",
		reachable:     true,
	})
	defer restore()

	runRouter, err := resolveRunRouter(context.Background(), io.Discard, cli.Command{})
	if err != nil {
		t.Fatal(err)
	}
	if runRouter.RouterLabel != "Windows" || !runRouter.Bridge {
		t.Fatalf("runRouter = %#v", runRouter)
	}
}

func TestResolveRunRouterUsesCompanionEvenWhenLocalWSLRouterLooksHealthy(t *testing.T) {
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:      true,
		healthErr:  nil,
		wslIP:      "172.20.10.2",
		reachable:  true,
		localAdmin: fakeAdminClient{},
	})
	defer restore()

	runRouter, err := resolveRunRouter(context.Background(), io.Discard, cli.Command{})
	if err != nil {
		t.Fatal(err)
	}
	if runRouter.RouterLabel != "Windows" || !runRouter.Bridge {
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

	_, err := resolveRunRouter(context.Background(), io.Discard, cli.Command{})
	if err == nil {
		t.Fatal("expected bridge reachability error")
	}
	if !strings.Contains(err.Error(), "Windows gohere authority could not reach the WSL dev-server listener") ||
		!strings.Contains(err.Error(), "gohere doctor") ||
		!strings.Contains(err.Error(), "Tried: 127.0.0.1, localhost, 172.20.10.2") {
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

	_, err := resolveRunRouter(context.Background(), io.Discard, cli.Command{})
	if err == nil {
		t.Fatal("expected bridge probe error")
	}
	if !strings.Contains(err.Error(), "probe endpoint failed") ||
		!strings.Contains(err.Error(), "gohere doctor") {
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

	runRouter, err := resolveRunRouter(context.Background(), io.Discard, cli.Command{})
	if err != nil {
		t.Fatal(err)
	}
	if runRouter.RouteTargetHost != "127.0.0.1" || runRouter.ChildHost != "127.0.0.1" || runRouter.RouterLabel != "Windows" {
		t.Fatalf("runRouter = %#v", runRouter)
	}
	wantProbes := []string{"127.0.0.1"}
	if !sameStrings(probes, wantProbes) {
		t.Fatalf("probes = %#v, want %#v", probes, wantProbes)
	}
}

func TestResolveRunRouterBindsAllInterfacesOnlyWhenWSLIPIsRequired(t *testing.T) {
	var probes []string
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:         true,
		token:         "windows-token",
		wslIP:         "172.20.10.2",
		windowsBinary: true,
		probeReachable: map[string]bool{
			"127.0.0.1":   false,
			"localhost":   false,
			"172.20.10.2": true,
		},
		probeHosts: &probes,
		admin:      &recordingAdminClient{},
	})
	defer restore()

	runRouter, err := resolveRunRouter(context.Background(), io.Discard, cli.Command{})
	if err != nil {
		t.Fatal(err)
	}
	if runRouter.RouteTargetHost != "172.20.10.2" || runRouter.ChildHost != "0.0.0.0" || runRouter.RouterLabel != "Windows" {
		t.Fatalf("runRouter = %#v", runRouter)
	}
	wantProbes := []string{"127.0.0.1", "localhost", "172.20.10.2"}
	if !sameStrings(probes, wantProbes) {
		t.Fatalf("probes = %#v, want %#v", probes, wantProbes)
	}
}

func TestRunWSLBridgeChoosesPackagePortForChildBindHost(t *testing.T) {
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:         true,
		token:         "windows-token",
		wslIP:         "172.20.10.2",
		windowsBinary: true,
		probeReachable: map[string]bool{
			"127.0.0.1":   false,
			"localhost":   false,
			"172.20.10.2": true,
		},
		admin: &recordingAdminClient{},
	})
	defer restore()

	oldChooseFreePortForHost := chooseFreePortForHostFunc
	oldStartRunner := startRunnerFunc
	defer func() {
		chooseFreePortForHostFunc = oldChooseFreePortForHost
		startRunnerFunc = oldStartRunner
	}()

	var chosenHosts []string
	chooseFreePortForHostFunc = func(host string) (int, error) {
		chosenHosts = append(chosenHosts, host)
		return 5173, nil
	}
	var got runner.Config
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		got = cfg
		return &runner.Result{Port: cfg.ChosenPort}, nil
	}

	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"dev":"vite"}}`,
	})
	if err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "dev"}, dir, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}

	if len(chosenHosts) == 0 || chosenHosts[len(chosenHosts)-1] != "0.0.0.0" {
		t.Fatalf("chosen hosts = %#v, want final selection on 0.0.0.0", chosenHosts)
	}
	assertEnv(t, got.Env, "HOST", "0.0.0.0")
	assertEnv(t, got.Env, "PORT", "5173")
	wantCommand := []string{"npm", "run", "dev", "--", "--host", "0.0.0.0", "--port", "5173", "--strictPort"}
	if !sameStrings(got.Command, wantCommand) {
		t.Fatalf("command = %#v, want %#v", got.Command, wantCommand)
	}
}

func TestRunWSLBridgeAddsHostWhenScriptAlreadyHasPort(t *testing.T) {
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:         true,
		token:         "windows-token",
		wslIP:         "172.20.10.2",
		windowsBinary: true,
		probeReachable: map[string]bool{
			"127.0.0.1":   false,
			"localhost":   false,
			"172.20.10.2": true,
		},
		admin: &recordingAdminClient{},
	})
	defer restore()

	oldStartRunner := startRunnerFunc
	oldChooseFreePortForHost := chooseFreePortForHostFunc
	defer func() {
		startRunnerFunc = oldStartRunner
		chooseFreePortForHostFunc = oldChooseFreePortForHost
	}()

	var got runner.Config
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		got = cfg
		return &runner.Result{Port: 3000}, nil
	}
	chooseFreePortForHostFunc = func(host string) (int, error) {
		return 5173, nil
	}

	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"dev":"vite --port 3000"}}`,
	})
	if err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "dev"}, dir, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}

	wantCommand := []string{"npm", "run", "dev", "--", "--host", "0.0.0.0"}
	if !sameStrings(got.Command, wantCommand) {
		t.Fatalf("command = %#v, want %#v", got.Command, wantCommand)
	}
	assertEnv(t, got.Env, "HOST", "0.0.0.0")
}

func TestRunWorkspaceWSLBridgeChoosesPortsForChildBindHost(t *testing.T) {
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:         true,
		token:         "windows-token",
		wslIP:         "172.20.10.2",
		windowsBinary: true,
		probeReachable: map[string]bool{
			"127.0.0.1":   false,
			"localhost":   false,
			"172.20.10.2": true,
		},
		admin: &recordingAdminClient{},
	})
	defer restore()

	oldChooseFreePortForHost := chooseFreePortForHostFunc
	oldStartRunner := startRunnerFunc
	defer func() {
		chooseFreePortForHostFunc = oldChooseFreePortForHost
		startRunnerFunc = oldStartRunner
	}()

	var chosenHosts []string
	nextPort := 5173
	chooseFreePortForHostFunc = func(host string) (int, error) {
		chosenHosts = append(chosenHosts, host)
		port := nextPort
		nextPort++
		return port, nil
	}
	var configs []runner.Config
	var configsMu sync.Mutex
	startRunnerFunc = func(ctx context.Context, cfg runner.Config) (*runner.Result, error) {
		configsMu.Lock()
		configs = append(configs, cfg)
		configsMu.Unlock()
		return &runner.Result{Port: cfg.ChosenPort}, nil
	}

	root := tempProject(t, map[string]string{
		"package.json":             `{"name":"ctrltube","workspaces":["apps/*"]}`,
		"pnpm-lock.yaml":           "",
		"apps/web/package.json":    `{"name":"@ctrltube/web","scripts":{"dev":"vite"}}`,
		"apps/worker/package.json": `{"name":"@ctrltube/worker","scripts":{"dev":"wrangler dev"}}`,
	})
	if err := Run(context.Background(), cli.Command{Kind: cli.CommandRun, Script: "dev"}, root, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}

	if !sameStrings(chosenHosts, []string{"0.0.0.0", "0.0.0.0"}) {
		t.Fatalf("chosen hosts = %#v, want two 0.0.0.0 selections", chosenHosts)
	}
	if len(configs) != 2 {
		t.Fatalf("configs = %#v, want two workspace services", configs)
	}
	for _, cfg := range configs {
		assertEnv(t, cfg.Env, "HOST", "0.0.0.0")
		command := strings.Join(cfg.Command, " ")
		if strings.Contains(command, "--host") && !strings.Contains(command, "0.0.0.0") {
			t.Fatalf("command = %#v, want child bind host when host args are injected", cfg.Command)
		}
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
	routes, err := admin.Routes(context.Background())
	if err != nil || len(routes) != 0 {
		t.Fatalf("static routes after cleanup = %#v, err = %v", routes, err)
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
		Host:        "vibe-oke.localhost",
		Target:      "http://127.0.0.1:46387",
		CWD:         "/tmp/vibe-oke",
		PID:         999999,
		Mode:        "package",
		ProjectName: "vibe",
		StartedAt:   time.Date(2026, 5, 28, 1, 2, 3, 0, time.UTC),
	}})
	var out strings.Builder

	if err := ListWithStore(&out, store, true); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "host") || !strings.Contains(text, "target") || !strings.Contains(text, "status") || !strings.Contains(text, "pid") || !strings.Contains(text, "cwd") {
		t.Fatalf("verbose list output = %q", text)
	}
	if !strings.Contains(text, "vibe-oke.localhost") || !strings.Contains(text, "dead") || !strings.Contains(text, "999999") || !strings.Contains(text, "/tmp/vibe-oke") {
		t.Fatalf("verbose list output = %q", text)
	}
	for _, want := range []string{"mode", "source", "owner", "started", "stop", "package", "local", "2026-05-28T01:02:03Z"} {
		if !strings.Contains(text, want) {
			t.Fatalf("verbose list output missing %q: %q", want, text)
		}
	}
	if strings.Contains(text, "backend") || strings.Contains(text, "cwd /tmp/vibe-oke") || strings.Contains(text, "pid 123") {
		t.Fatalf("verbose list output = %q", text)
	}
}

func TestListJSONOutput(t *testing.T) {
	ownerEnv := foreignTestOwnerEnv()
	store := router.NewMemoryStore()
	store.Save([]router.Route{{
		Host:           "vibe-oke.localhost",
		Target:         "://bad-url",
		CWD:            "/tmp/vibe-oke",
		PID:            123,
		Mode:           "static",
		Source:         "wsl",
		OwnerEnv:       ownerEnv,
		OwnerInstance:  "owner-1",
		Distro:         "Ubuntu",
		LinuxUser:      "alice",
		RunnerID:       "runner-1",
		ListenTarget:   "http://0.0.0.0:3000",
		LeaseExpiresAt: time.Date(2026, 5, 28, 1, 3, 33, 0, time.UTC),
		ProjectName:    "vibe",
		Name:           "vibe-oke",
		StartedAt:      time.Date(2026, 5, 28, 1, 2, 3, 0, time.UTC),
	}})
	var out strings.Builder

	if err := ListWithStoreOptions(&out, store, ListOptions{JSON: true}); err != nil {
		t.Fatal(err)
	}
	var routes []listRoute
	if err := json.Unmarshal([]byte(out.String()), &routes); err != nil {
		t.Fatalf("list json did not parse: %v\n%s", err, out.String())
	}
	if len(routes) != 1 {
		t.Fatalf("routes = %#v, want one route", routes)
	}
	route := routes[0]
	if route.Host != "vibe-oke.localhost" ||
		route.Target != "://bad-url" ||
		route.Status != string(lifecycle.RouteStatusUnknown) ||
		route.PID != 123 ||
		route.CWD != "/tmp/vibe-oke" ||
		route.Mode != "static" ||
		route.Source != "wsl" ||
		route.OwnerEnv != ownerEnv ||
		route.OwnerInstance != "owner-1" ||
		route.Distro != "Ubuntu" ||
		route.LinuxUser != "alice" ||
		route.RunnerID != "runner-1" ||
		route.ListenTarget != "http://0.0.0.0:3000" ||
		route.LeaseExpiresAt != "2026-05-28T01:03:33Z" ||
		route.ProjectName != "vibe" ||
		route.Name != "vibe-oke" ||
		route.StartedAt != "2026-05-28T01:02:03Z" {
		t.Fatalf("route = %#v", route)
	}
	if route.CanStop {
		t.Fatalf("route canStop = true, want false for foreign route: %#v", route)
	}
	if !strings.Contains(route.StopReason, "Ubuntu/alice") ||
		!strings.Contains(route.StopReason, "that WSL owner") {
		t.Fatalf("stopReason = %q", route.StopReason)
	}
}

func foreignTestOwnerEnv() string {
	if runOwnerEnv() == "windows" {
		return "wsl"
	}
	return "windows"
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
	if err := List(t.Context(), &out, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "web.localhost") {
		t.Fatalf("list output = %q", out.String())
	}
}

func TestListUsesServiceComputedRouteStatusFromWSL(t *testing.T) {
	admin := &recordingAdminClient{
		routes: []router.Route{{
			Host:   "squawk.localhost",
			Target: "http://127.0.0.1:57605",
			CWD:    `D:\roie\dev\web\squawk`,
		}},
		statuses: []router.RouteStatus{{
			Route: router.Route{
				Host:   "squawk.localhost",
				Target: "http://127.0.0.1:57605",
				CWD:    `D:\roie\dev\web\squawk`,
			},
			Status: "ready",
		}},
	}
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:         true,
		token:         "windows-token",
		windowsBinary: true,
		admin:         admin,
	})
	defer restore()

	var out strings.Builder
	if err := List(t.Context(), &out, true); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "squawk.localhost") || !strings.Contains(text, "ready") {
		t.Fatalf("list output = %q", text)
	}
	if strings.Contains(text, "dead") {
		t.Fatalf("list used client-side target status: %q", text)
	}
}

func TestListRefinesUnknownServiceStatusForWSLOwnedRouteFromWSL(t *testing.T) {
	restoreMetadata := stubCurrentWSLMetadata(t)
	defer restoreMetadata()
	oldDetectWSL := detectWSLFunc
	defer func() {
		detectWSLFunc = oldDetectWSL
	}()
	detectWSLFunc = func() bool { return true }
	route := router.Route{
		Host:           "old-wsl.localhost",
		Target:         "http://127.0.0.1:1",
		PID:            999999,
		CWD:            "/home/roie/dev/old-wsl",
		Source:         "wsl",
		OwnerEnv:       "wsl",
		OwnerInstance:  "test-owner",
		Distro:         "Ubuntu",
		LinuxUser:      "alice",
		LeaseExpiresAt: time.Now().Add(-time.Minute),
	}
	admin := &recordingAdminClient{
		routes:   []router.Route{route},
		statuses: []router.RouteStatus{{Route: route, Status: "unknown"}},
	}

	statuses, err := adminRouteStatuses(context.Background(), admin)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Route.Host != "old-wsl.localhost" || statuses[0].Status != lifecycle.RouteStatusDead {
		t.Fatalf("statuses = %#v, want refined dead WSL-owned route", statuses)
	}
}

func TestListFallsBackToServiceProbeWhenRouteStatusesEndpointIsMissing(t *testing.T) {
	route := router.Route{
		Host:   "squawk.localhost",
		Target: "http://127.0.0.1:57605",
		CWD:    `D:\roie\dev\web\squawk`,
	}
	admin := &recordingAdminClient{
		routes:         []router.Route{route},
		statusErr:      admin.ErrRouteStatusesUnsupported,
		probeReachable: map[string]bool{route.Target: true},
	}
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:         true,
		token:         "windows-token",
		windowsBinary: true,
		admin:         admin,
	})
	defer restore()

	var out strings.Builder
	if err := List(t.Context(), &out, true); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "squawk.localhost") || !strings.Contains(text, "ready") {
		t.Fatalf("list output = %q", text)
	}
}

func TestListDoesNotFallbackToProbeForRouteStatusServerError(t *testing.T) {
	route := router.Route{
		Host:   "squawk.localhost",
		Target: "http://127.0.0.1:57605",
		CWD:    `D:\roie\dev\web\squawk`,
	}
	admin := &recordingAdminClient{
		routes:         []router.Route{route},
		statusErr:      errors.New("GET /route-statuses returned 500 Internal Server Error"),
		probeReachable: map[string]bool{route.Target: true},
	}
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:         true,
		token:         "windows-token",
		windowsBinary: true,
		admin:         admin,
	})
	defer restore()

	var out strings.Builder
	err := List(t.Context(), &out, true)
	if err == nil || !strings.Contains(err.Error(), "500 Internal Server Error") {
		t.Fatalf("List error = %v", err)
	}
}

func TestListFallbackWithoutProbeKeepsStatusUnknown(t *testing.T) {
	route := router.Route{
		Host:   "old-local.localhost",
		Target: "http://127.0.0.1:1",
		PID:    999999,
		CWD:    "/tmp/old-local",
	}
	client := routeStatusesUnsupportedClient{routes: []router.Route{route}}

	statuses, err := adminRouteStatuses(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Route.Host != "old-local.localhost" || statuses[0].Status != lifecycle.RouteStatusUnknown {
		t.Fatalf("statuses = %#v", statuses)
	}
}

func TestListFallbackWithoutProbeClassifiesWSLOwnedRoutesFromWSL(t *testing.T) {
	restoreMetadata := stubCurrentWSLMetadata(t)
	defer restoreMetadata()
	oldDetectWSL := detectWSLFunc
	defer func() {
		detectWSLFunc = oldDetectWSL
	}()
	detectWSLFunc = func() bool { return true }
	route := router.Route{
		Host:          "old-wsl.localhost",
		Target:        "http://127.0.0.1:1",
		PID:           999999,
		CWD:           "/home/roie/dev/old-wsl",
		Source:        "wsl",
		OwnerEnv:      "wsl",
		OwnerInstance: "test-owner",
		Distro:        "Ubuntu",
		LinuxUser:     "alice",
	}
	client := routeStatusesUnsupportedClient{routes: []router.Route{route}}

	statuses, err := adminRouteStatuses(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Route.Host != "old-wsl.localhost" || statuses[0].Status != lifecycle.RouteStatusDead {
		t.Fatalf("statuses = %#v, want dead WSL-owned route", statuses)
	}
}

func TestListFallbackWithoutProbeKeepsForeignRoutesUnknownFromWSL(t *testing.T) {
	oldDetectWSL := detectWSLFunc
	defer func() {
		detectWSLFunc = oldDetectWSL
	}()
	detectWSLFunc = func() bool { return true }
	route := router.Route{
		Host:     "old-windows.localhost",
		Target:   "http://127.0.0.1:1",
		PID:      999999,
		CWD:      `D:\dev\old-windows`,
		OwnerEnv: "windows",
	}
	client := routeStatusesUnsupportedClient{routes: []router.Route{route}}

	statuses, err := adminRouteStatuses(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Route.Host != "old-windows.localhost" || statuses[0].Status != lifecycle.RouteStatusUnknown {
		t.Fatalf("statuses = %#v, want unknown foreign route", statuses)
	}
}

func TestPruneUsesWindowsRouterFromWSL(t *testing.T) {
	route := router.Route{
		Host:   "dead.localhost",
		Target: "http://127.0.0.1:1",
		PID:    999999,
		CWD:    "/home/roie/dev/dead",
	}
	admin := &recordingAdminClient{
		routes:   []router.Route{route},
		statuses: []router.RouteStatus{{Route: route, Status: "dead"}},
	}
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:         true,
		token:         "windows-token",
		windowsBinary: true,
		admin:         admin,
	})
	defer restore()

	var out strings.Builder
	if err := Prune(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if admin.deleted != "dead.localhost" {
		t.Fatalf("deleted = %q, want dead.localhost", admin.deleted)
	}
	if out.String() != "Removed 1 dead route.\n" {
		t.Fatalf("prune output = %q", out.String())
	}
}

func TestPruneKeepsServiceReadyWindowsLoopbackRouteFromWSL(t *testing.T) {
	route := router.Route{
		Host:   "squawk.localhost",
		Target: "http://127.0.0.1:57605",
		PID:    21480,
		CWD:    `D:\roie\dev\web\squawk`,
	}
	admin := &recordingAdminClient{
		routes:   []router.Route{route},
		statuses: []router.RouteStatus{{Route: route, Status: "ready"}},
	}
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:         true,
		token:         "windows-token",
		windowsBinary: true,
		admin:         admin,
	})
	defer restore()

	var out strings.Builder
	if err := Prune(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if admin.deleted != "" {
		t.Fatalf("deleted = %q, want no delete", admin.deleted)
	}
	if out.String() != "No dead routes.\n" {
		t.Fatalf("prune output = %q", out.String())
	}
}

func TestPruneFallbackProbeKeepsUnreachableRouteUnknown(t *testing.T) {
	route := router.Route{
		Host:   "maybe.localhost",
		Target: "http://127.0.0.1:59999",
		CWD:    `D:\roie\dev\web\maybe`,
	}
	admin := &recordingAdminClient{
		routes:         []router.Route{route},
		statusErr:      admin.ErrRouteStatusesUnsupported,
		probeReachable: map[string]bool{route.Target: false},
	}
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:         true,
		token:         "windows-token",
		windowsBinary: true,
		admin:         admin,
	})
	defer restore()

	var out strings.Builder
	if err := Prune(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if admin.deleted != "" {
		t.Fatalf("deleted = %q, want no delete", admin.deleted)
	}
	if out.String() != "No dead routes.\n" {
		t.Fatalf("prune output = %q", out.String())
	}
}

func TestPruneFallbackProbeRemovesDeadWSLOwnedRouteFromWSL(t *testing.T) {
	restoreMetadata := stubCurrentWSLMetadata(t)
	defer restoreMetadata()
	oldDetectWSL := detectWSLFunc
	defer func() {
		detectWSLFunc = oldDetectWSL
	}()
	detectWSLFunc = func() bool { return true }
	route := router.Route{
		Host:           "old-wsl.localhost",
		Target:         "http://127.0.0.1:1",
		PID:            999999,
		CWD:            "/home/roie/dev/old-wsl",
		Source:         "wsl",
		OwnerEnv:       "wsl",
		OwnerInstance:  "test-owner",
		Distro:         "Ubuntu",
		LinuxUser:      "alice",
		LeaseExpiresAt: time.Now().Add(-time.Minute),
	}
	admin := &recordingAdminClient{
		routes:         []router.Route{route},
		statusErr:      admin.ErrRouteStatusesUnsupported,
		probeReachable: map[string]bool{route.Target: false},
	}

	removed, err := pruneAdminRoutes(context.Background(), admin)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 || admin.deleted != "old-wsl.localhost" {
		t.Fatalf("removed=%d deleted=%q, want old WSL route removed", removed, admin.deleted)
	}
}

func TestAdminProbeRouteStatusesStopsWhenContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := routeStatusesUnsupportedClient{routes: []router.Route{{
		Host:   "app.localhost",
		Target: "http://127.0.0.1:3000",
	}}}
	probe := &recordingProbeClient{}

	_, err := adminProbeRouteStatuses(ctx, client, probe)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("adminProbeRouteStatuses error = %v, want context.Canceled", err)
	}
	if probe.calls != 0 {
		t.Fatalf("probe calls = %d, want 0", probe.calls)
	}
}

func TestPruneAdminRoutesReportsPartialDeletion(t *testing.T) {
	routes := []router.Route{
		{Host: "one.localhost", Target: "http://127.0.0.1:1"},
		{Host: "two.localhost", Target: "http://127.0.0.1:2"},
	}
	client := &recordingAdminClient{
		statuses: []router.RouteStatus{
			{Route: routes[0], Status: "dead"},
			{Route: routes[1], Status: "dead"},
		},
		deleteErr: errors.New("delete failed"),
	}

	removed, err := pruneAdminRoutes(context.Background(), client)
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if err == nil || !strings.Contains(err.Error(), "removed 1 dead route") || !strings.Contains(err.Error(), "two.localhost") {
		t.Fatalf("pruneAdminRoutes error = %v", err)
	}
}

func TestRouteManagerDoesNotReadWindowsTokenWhenCompanionIsAvailable(t *testing.T) {
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:         true,
		windowsBinary: true,
	})
	defer restore()

	manager, err := resolveRouteManager(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if manager.Client == nil || !manager.RouterReady {
		t.Fatalf("manager = %#v", manager)
	}
}

func TestRouteManagerUsesCompanionWhenLocalWSLRouterLooksHealthy(t *testing.T) {
	companionAdmin := &recordingAdminClient{routes: []router.Route{{Host: "windows.localhost"}}}
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:      true,
		healthErr:  nil,
		admin:      companionAdmin,
		localAdmin: fakeAdminClient{},
	})
	defer restore()

	manager, err := resolveRouteManager(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if manager.Client == nil || !manager.RouterReady {
		t.Fatalf("manager = %#v", manager)
	}
	routes, err := manager.Client.Routes(t.Context())
	if err != nil || len(routes) != 1 || routes[0].Host != "windows.localhost" {
		t.Fatalf("routes = %#v, err = %v", routes, err)
	}
}

func TestRouteManagerReportsWindowsStartFailureWhenInstalled(t *testing.T) {
	startCalls := 0
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:             true,
		token:             "windows-token",
		healthErr:         errors.New("connection refused"),
		windowsBinary:     true,
		startWindowsErr:   errors.New("start failed"),
		startWindowsCalls: &startCalls,
	})
	defer restore()

	_, err := resolveRouteManager(context.Background())
	if err == nil {
		t.Fatal("expected Windows start failure")
	}
	if startCalls != 1 {
		t.Fatalf("start calls = %d, want 1", startCalls)
	}
	if !strings.Contains(err.Error(), "could not use the Windows authority from WSL") ||
		!strings.Contains(err.Error(), "No WSL router was started") ||
		!strings.Contains(err.Error(), "start failed") {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestRouteManagerStartsWindowsServiceFromWSLWhenInstalledButStopped(t *testing.T) {
	startCalls := 0
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:                    true,
		token:                    "windows-token",
		healthErr:                errors.New("connection refused"),
		windowsBinary:            true,
		startWindowsCalls:        &startCalls,
		startWindowsMakesHealthy: true,
		admin:                    &recordingAdminClient{},
	})
	defer restore()

	manager, err := resolveRouteManager(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if startCalls != 1 {
		t.Fatalf("start calls = %d, want 1", startCalls)
	}
	if manager.Client == nil || !manager.RouterReady {
		t.Fatalf("manager = %#v", manager)
	}
}

func TestRouteManagerDoesNotFallBackToLocalRouterWhenWindowsAuthorityIsUnavailable(t *testing.T) {
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:      true,
		token:      "windows-token",
		healthErr:  errors.New("connection refused"),
		localAdmin: fakeAdminClient{},
	})
	defer restore()

	_, err := resolveRouteManager(context.Background())
	if err == nil || !strings.Contains(err.Error(), "No WSL router was started") ||
		!strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("error = %v", err)
	}
}

func TestRouteManagerIgnoresStaleTokenAndLocalRouterWhenCompanionWorks(t *testing.T) {
	companionAdmin := &recordingAdminClient{routes: []router.Route{{Host: "windows.localhost"}}}
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:      true,
		token:      "windows-token",
		healthErr:  nil,
		admin:      companionAdmin,
		localAdmin: fakeAdminClient{},
	})
	defer restore()

	manager, err := resolveRouteManager(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if manager.Client == nil || !manager.RouterReady {
		t.Fatalf("manager = %#v", manager)
	}
	routes, err := manager.Client.Routes(t.Context())
	if err != nil || len(routes) != 1 || routes[0].Host != "windows.localhost" {
		t.Fatalf("routes = %#v, err = %v", routes, err)
	}
}

func TestStopUsesWindowsRouterFromWSL(t *testing.T) {
	cwd := "/home/roie/dev/web"
	admin := &recordingAdminClient{routes: []router.Route{{
		Host:          "web.localhost",
		Target:        "http://172.20.10.2:5173",
		CWD:           cwd,
		OwnerCWD:      cwd,
		OwnerEnv:      "wsl",
		OwnerInstance: "test-owner",
		Distro:        "Ubuntu",
		LinuxUser:     "alice",
	}}}
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:         true,
		token:         "windows-token",
		windowsBinary: true,
		admin:         admin,
	})
	defer restore()

	var out strings.Builder
	if err := Stop(t.Context(), cwd, &out); err != nil {
		t.Fatal(err)
	}
	if admin.deleted != "web.localhost" {
		t.Fatalf("deleted = %q, want web.localhost", admin.deleted)
	}
	if out.String() != "Stopped web.localhost.\n" {
		t.Fatalf("stop output = %q", out.String())
	}
}

func TestStopAtWorkspaceRootStopsWorkspacePackageRoutes(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
	}()

	root := tempProject(t, map[string]string{
		"package.json":             `{"name":"ctrltube","workspaces":["apps/*"]}`,
		"apps/web/package.json":    `{"name":"@ctrltube/web","scripts":{"dev":"vite"}}`,
		"apps/worker/package.json": `{"name":"@ctrltube/worker","scripts":{"dev":"wrangler dev"}}`,
	})
	webDir := filepath.Join(root, "apps", "web")
	workerDir := filepath.Join(root, "apps", "worker")
	admin := &multiRecordingAdminClient{routes: []router.Route{
		{Host: "web.ctrltube.localhost", CWD: webDir, OwnerCWD: webDir},
		{Host: "worker.ctrltube.localhost", CWD: workerDir, OwnerCWD: workerDir},
	}}
	defaultAdminClientFunc = func() (adminClient, error) {
		return admin, nil
	}

	var out strings.Builder
	if err := Stop(t.Context(), root, &out); err != nil {
		t.Fatal(err)
	}

	wantDeleted := []string{"web.ctrltube.localhost", "worker.ctrltube.localhost"}
	if !sameStrings(admin.deletedHosts(), wantDeleted) {
		t.Fatalf("deleted hosts = %#v, want %#v", admin.deletedHosts(), wantDeleted)
	}
	wantOut := "Stopped web.ctrltube.localhost.\nStopped worker.ctrltube.localhost.\n"
	if out.String() != wantOut {
		t.Fatalf("stop output = %q, want %q", out.String(), wantOut)
	}
}

func TestStopTargetProjectStopsProjectRoutes(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
	}()

	admin := &multiRecordingAdminClient{routes: []router.Route{
		{Host: "web.ctrltube.localhost", Name: "web", ProjectName: "ctrltube"},
		{Host: "worker.ctrltube.localhost", Name: "worker", ProjectName: "ctrltube"},
		{Host: "api.other.localhost", Name: "api", ProjectName: "other"},
	}}
	defaultAdminClientFunc = func() (adminClient, error) {
		return admin, nil
	}

	var out strings.Builder
	cmd := cli.Command{Kind: cli.CommandStop, StopTarget: "ctrltube"}
	if err := StopWithCommand(t.Context(), cmd, t.TempDir(), &out); err != nil {
		t.Fatal(err)
	}

	wantDeleted := []string{"web.ctrltube.localhost", "worker.ctrltube.localhost"}
	if !sameStrings(admin.deletedHosts(), wantDeleted) {
		t.Fatalf("deleted hosts = %#v, want %#v", admin.deletedHosts(), wantDeleted)
	}
	wantOut := "Stopped web.ctrltube.localhost.\nStopped worker.ctrltube.localhost.\n"
	if out.String() != wantOut {
		t.Fatalf("stop output = %q, want %q", out.String(), wantOut)
	}
}

func TestStopTargetHostWithoutLocalhostStopsOneRoute(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
	}()

	admin := &multiRecordingAdminClient{routes: []router.Route{
		{Host: "web.ctrltube.localhost", Name: "web", ProjectName: "ctrltube"},
		{Host: "worker.ctrltube.localhost", Name: "worker", ProjectName: "ctrltube"},
	}}
	defaultAdminClientFunc = func() (adminClient, error) {
		return admin, nil
	}

	var out strings.Builder
	cmd := cli.Command{Kind: cli.CommandStop, StopTarget: "web.ctrltube"}
	if err := StopWithCommand(t.Context(), cmd, t.TempDir(), &out); err != nil {
		t.Fatal(err)
	}

	if !sameStrings(admin.deletedHosts(), []string{"web.ctrltube.localhost"}) {
		t.Fatalf("deleted hosts = %#v, want web route", admin.deletedHosts())
	}
	if out.String() != "Stopped web.ctrltube.localhost.\n" {
		t.Fatalf("stop output = %q", out.String())
	}
}

func TestStopTargetDeleteFailureIncludesRouteCleanupDetails(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
	}()

	admin := &multiRecordingAdminClient{
		routes: []router.Route{{
			Host: "web.ctrltube.localhost", Name: "web", ProjectName: "ctrltube",
		}},
		deleteErr: errors.New("delete failed"),
	}
	defaultAdminClientFunc = func() (adminClient, error) {
		return admin, nil
	}

	var out strings.Builder
	cmd := cli.Command{Kind: cli.CommandStop, StopTarget: "web"}
	err := StopWithCommand(t.Context(), cmd, t.TempDir(), &out)
	if err == nil {
		t.Fatal("expected delete failure")
	}
	for _, want := range []string{
		"could not stop web.ctrltube.localhost",
		"could not delete route web.ctrltube.localhost",
		"route may still appear in gohere list",
		"delete failed",
		"gohere doctor",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want to contain %q", err.Error(), want)
		}
	}
	if !sameStrings(admin.deletedHosts(), []string{"web.ctrltube.localhost"}) {
		t.Fatalf("deleted hosts = %#v, want failed delete attempt", admin.deletedHosts())
	}
	if out.String() != "" {
		t.Fatalf("stdout = %q, want empty", out.String())
	}
}

func TestStopTargetAmbiguousProjectAndRouteNameErrors(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
	}()

	admin := &multiRecordingAdminClient{routes: []router.Route{
		{Host: "web.ctrltube.localhost", Name: "web", ProjectName: "ctrltube"},
		{Host: "worker.ctrltube.localhost", Name: "worker", ProjectName: "ctrltube"},
		{Host: "ctrltube.tools.localhost", Name: "ctrltube", ProjectName: "tools"},
	}}
	defaultAdminClientFunc = func() (adminClient, error) {
		return admin, nil
	}

	var out strings.Builder
	cmd := cli.Command{Kind: cli.CommandStop, StopTarget: "ctrltube"}
	err := StopWithCommand(t.Context(), cmd, t.TempDir(), &out)
	if err == nil {
		t.Fatal("expected ambiguity error")
	}
	if !strings.Contains(err.Error(), `"ctrltube" matches a project and a route`) ||
		!strings.Contains(err.Error(), "web.ctrltube.localhost") ||
		!strings.Contains(err.Error(), "ctrltube.tools.localhost") {
		t.Fatalf("error = %q", err.Error())
	}
	if len(admin.deletedHosts()) != 0 {
		t.Fatalf("deleted hosts = %#v, want none", admin.deletedHosts())
	}
	if out.String() != "" {
		t.Fatalf("stdout = %q, want empty", out.String())
	}
}

func TestStopTargetNoMatchReportsNoMatchingRoute(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
	}()

	admin := &multiRecordingAdminClient{routes: []router.Route{{
		Host: "web.ctrltube.localhost", Name: "web", ProjectName: "ctrltube",
	}}}
	defaultAdminClientFunc = func() (adminClient, error) {
		return admin, nil
	}

	var out strings.Builder
	cmd := cli.Command{Kind: cli.CommandStop, StopTarget: "missing"}
	if err := StopWithCommand(t.Context(), cmd, t.TempDir(), &out); err != nil {
		t.Fatal(err)
	}
	if len(admin.deletedHosts()) != 0 {
		t.Fatalf("deleted hosts = %#v, want none", admin.deletedHosts())
	}
	if out.String() != "No matching gohere route found.\n" {
		t.Fatalf("stop output = %q", out.String())
	}
}

func TestStopAllStopsDeadRoutesAndSkipsUnverifiedLiveRoutes(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
	}()

	admin := &multiRecordingAdminClient{routes: []router.Route{
		{Host: "dead.localhost", Name: "dead"},
		{Host: "live.localhost", Name: "live", PID: os.Getpid()},
	}}
	defaultAdminClientFunc = func() (adminClient, error) {
		return admin, nil
	}

	var out strings.Builder
	cmd := cli.Command{Kind: cli.CommandStop, StopAll: true}
	if err := StopWithCommand(t.Context(), cmd, t.TempDir(), &out); err != nil {
		t.Fatal(err)
	}

	if !sameStrings(admin.deletedHosts(), []string{"dead.localhost"}) {
		t.Fatalf("deleted hosts = %#v, want dead route only", admin.deletedHosts())
	}
	text := out.String()
	if !strings.Contains(text, "Stopped dead.localhost.") ||
		!strings.Contains(text, "Skipped live.localhost: could not verify the original gohere process.") {
		t.Fatalf("stop output = %q", text)
	}
}

func TestStopStoreRoutesPreservesRoutesAddedBetweenLoadAndSave(t *testing.T) {
	dead := router.Route{Host: "dead.localhost", Target: "http://127.0.0.1:5173", PID: 999999}
	added := router.Route{Host: "added.localhost", Target: "://bad-url"}
	store := &interleavingAppRouteStore{
		routes: []router.Route{dead},
		added:  added,
	}

	result, err := stopStoreRoutes(store, []router.Route{dead})
	if err != nil {
		t.Fatal(err)
	}
	if !sameStrings(result.Hosts, []string{"dead.localhost"}) {
		t.Fatalf("stopped hosts = %#v, want dead route", result.Hosts)
	}
	if !store.usedUpdate {
		t.Fatal("stopStoreRoutes did not use transactional store update")
	}
	routes, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0].Host != "added.localhost" {
		t.Fatalf("routes = %#v, want concurrently added route preserved", routes)
	}
}

func TestDoctorWithStoreReportsActiveRouteCount(t *testing.T) {
	restoreLocalhostHTTP := stubLocalhostHTTPStatus(t, LocalhostHTTPStatus{OK: true, Detail: "reached gohere router"})
	defer restoreLocalhostHTTP()

	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, "router.pid"), []byte("12345\n"), 0600); err != nil {
		t.Fatal(err)
	}
	store := router.NewMemoryStore()
	store.Save([]router.Route{{Host: "app.localhost", Target: "http://127.0.0.1:1234"}})
	var out strings.Builder

	if err := DoctorWithStore(t.Context(), &out, stateDir, store, fakeAdminClient{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ok active routes 1") {
		t.Fatalf("doctor output = %q", out.String())
	}
	if !strings.Contains(out.String(), "ok service pid 12345") {
		t.Fatalf("doctor output = %q", out.String())
	}
}

func TestDoctorReportsInvalidTokenFormat(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, "token"), []byte("not-a-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder

	if err := DoctorWithChecks(t.Context(), &out, stateDir, router.NewMemoryStore(), fakeAdminClient{}, DoctorChecks{
		Port80Available: func() bool { return true },
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "fail token format invalid") {
		t.Fatalf("doctor output = %q", out.String())
	}
}

func TestDoctorReportsRouteStoreCorruption(t *testing.T) {
	var out strings.Builder

	if err := DoctorWithChecks(t.Context(), &out, t.TempDir(), brokenRouteStore{err: errors.New("invalid character")}, fakeAdminClient{}, DoctorChecks{
		Port80Available: func() bool { return true },
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "fail route store invalid character") {
		t.Fatalf("doctor output = %q", out.String())
	}
}

func TestDoctorReportsHTTPSDisabled(t *testing.T) {
	stateDir := t.TempDir()
	var out strings.Builder

	if err := DoctorWithChecks(t.Context(), &out, stateDir, router.NewMemoryStore(), fakeAdminClient{}, DoctorChecks{}); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out.String(), "fail https config disabled") {
		t.Fatalf("doctor output = %q", out.String())
	}
}

func TestDoctorReportsHTTPSCertificateAuthority(t *testing.T) {
	stateDir := t.TempDir()
	if err := appconfig.Save(stateDir, appconfig.Config{HTTPS: true}); err != nil {
		t.Fatal(err)
	}
	ca, err := localcert.Store{StateDir: stateDir}.EnsureCA()
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder

	if err := DoctorWithChecks(t.Context(), &out, stateDir, router.NewMemoryStore(), fakeAdminClient{}, DoctorChecks{}); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out.String(), "ok https certificate authority "+ca.Fingerprint) {
		t.Fatalf("doctor output = %q", out.String())
	}
}

func TestHTTPSDoctorChecksReportWindowsTrustFromWSL(t *testing.T) {
	oldDetectWSL := detectWSLFunc
	oldWindowsTrust := windowsHTTPSCATrustedFunc
	defer func() {
		detectWSLFunc = oldDetectWSL
		windowsHTTPSCATrustedFunc = oldWindowsTrust
	}()
	detectWSLFunc = func() bool { return true }

	stateDir := t.TempDir()
	if err := appconfig.Save(stateDir, appconfig.Config{HTTPS: true}); err != nil {
		t.Fatal(err)
	}
	store := localcert.Store{StateDir: stateDir}
	if _, err := store.EnsureCA(); err != nil {
		t.Fatal(err)
	}
	wantFingerprint, err := store.TrustFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	var gotFingerprint string
	windowsHTTPSCATrustedFunc = func(ctx context.Context, fingerprint string) (bool, string) {
		gotFingerprint = fingerprint
		return true, "trusted"
	}

	out := lifecycle.FormatDoctor(httpsDoctorChecks(t.Context(), stateDir))
	if gotFingerprint != wantFingerprint {
		t.Fatalf("windows trust fingerprint = %q, want %q", gotFingerprint, wantFingerprint)
	}
	if !strings.Contains(out, "ok windows https trust trusted") {
		t.Fatalf("doctor output = %q", out)
	}
}

func TestHTTPSDoctorChecksReportMissingWindowsTrustFromWSL(t *testing.T) {
	oldDetectWSL := detectWSLFunc
	oldWindowsTrust := windowsHTTPSCATrustedFunc
	defer func() {
		detectWSLFunc = oldDetectWSL
		windowsHTTPSCATrustedFunc = oldWindowsTrust
	}()
	detectWSLFunc = func() bool { return true }

	stateDir := t.TempDir()
	if err := appconfig.Save(stateDir, appconfig.Config{HTTPS: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := (localcert.Store{StateDir: stateDir}).EnsureCA(); err != nil {
		t.Fatal(err)
	}
	windowsHTTPSCATrustedFunc = func(ctx context.Context, fingerprint string) (bool, string) {
		return false, "missing"
	}

	out := lifecycle.FormatDoctor(httpsDoctorChecks(t.Context(), stateDir))
	if !strings.Contains(out, "fail windows https trust missing") ||
		!strings.Contains(out, "Run gohere again to repair Windows browser trust.") {
		t.Fatalf("doctor output = %q", out)
	}
}

func TestDoctorReportsDeadServicePID(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, "router.pid"), []byte("999999\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder

	if err := DoctorWithChecks(t.Context(), &out, stateDir, router.NewMemoryStore(), fakeAdminClient{}, DoctorChecks{
		Port80Available: func() bool { return true },
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "fail service process dead") {
		t.Fatalf("doctor output = %q", out.String())
	}
}

func TestDoctorWithStoreReportsLocalhostHTTPProbe(t *testing.T) {
	restoreLocalhostHTTP := stubLocalhostHTTPStatus(t, LocalhostHTTPStatus{OK: true, Detail: "reached gohere router"})
	defer restoreLocalhostHTTP()

	var out strings.Builder
	if err := DoctorWithStore(t.Context(), &out, t.TempDir(), router.NewMemoryStore(), fakeAdminClient{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ok .localhost routing reached gohere router") {
		t.Fatalf("doctor output = %q", out.String())
	}
}

func TestDoctorReportsUnavailableAdminAPIWhenClientMissing(t *testing.T) {
	var out strings.Builder

	if err := DoctorWithChecks(t.Context(), &out, t.TempDir(), router.NewMemoryStore(), nil, DoctorChecks{
		Port80Available: func() bool { return true },
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "fail admin API health unavailable") {
		t.Fatalf("doctor output = %q", out.String())
	}
}

func TestDoctorWithChecksShowsLocalhostHTTPProbeHint(t *testing.T) {
	var out strings.Builder
	if err := DoctorWithChecks(t.Context(), &out, t.TempDir(), router.NewMemoryStore(), nil, DoctorChecks{
		Port80Available: func() bool { return true },
		LocalhostHTTPStatus: func(context.Context) LocalhostHTTPStatus {
			return LocalhostHTTPStatus{
				OK:     false,
				Detail: "unreachable: dial tcp",
				Hint:   "Run gohere doctor from Windows too.",
			}
		},
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "fail .localhost routing unreachable: dial tcp\n  Run gohere doctor from Windows too.\n") {
		t.Fatalf("doctor output = %q", out.String())
	}
}

func TestLocalhostHTTPStatusRecognizesGohereMissingRoute(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, "<!doctype html><title>gohere route missing</title><h1>No gohere route is running</h1>")
	}))
	defer server.Close()

	status := localhostHTTPStatusForURL(t.Context(), server.URL)
	if !status.OK || status.Detail != "reached gohere router" {
		t.Fatalf("status = %#v, want gohere router reached", status)
	}
}

func TestLocalhostHTTPStatusRejectsUnexpectedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "not gohere")
	}))
	defer server.Close()

	status := localhostHTTPStatusForURL(t.Context(), server.URL)
	if status.OK {
		t.Fatalf("status = %#v, want failure", status)
	}
	if status.Detail != "unexpected response: 200 OK" ||
		!strings.Contains(status.Hint, "Another process may own port 80") ||
		!strings.Contains(status.Hint, "Windows/WSL") {
		t.Fatalf("status = %#v", status)
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
	if err := DoctorWithChecks(t.Context(), &out, t.TempDir(), router.NewMemoryStore(), fakeAdminClient{}, DoctorChecks{
		Port80Available: func() bool { return true },
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ok environment WSL") ||
		!strings.Contains(out.String(), "ok windows service available") {
		t.Fatalf("doctor output = %q", out.String())
	}
}

func TestDoctorSuppressesLocalWSLServiceChecksWhenWindowsBridgeAvailable(t *testing.T) {
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:         true,
		token:         "windows-token",
		windowsBinary: true,
		admin:         &recordingAdminClient{},
	})
	defer restore()

	var out strings.Builder
	if err := DoctorWithChecks(t.Context(), &out, t.TempDir(), router.NewMemoryStore(), fakeAdminClient{}, DoctorChecks{
		GOOS:            "linux",
		Port80Available: func() bool { return false },
		SystemdUserServiceOK: func() (bool, bool) {
			return true, false
		},
	}); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "ok local service not required; using Windows service") ||
		!strings.Contains(text, "ok windows service available") {
		t.Fatalf("doctor output = %q", text)
	}
	for _, unwanted := range []string{"fail service pid", "fail systemd user service", "ok setcap"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("doctor output should not contain %q: %q", unwanted, text)
		}
	}
}

func TestDoctorUsesCompanionInsteadOfProfileInstallHeuristicsFromWSL(t *testing.T) {
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL: true,
	})
	defer restore()

	var out strings.Builder
	if err := DoctorWithChecks(t.Context(), &out, t.TempDir(), router.NewMemoryStore(), fakeAdminClient{}, DoctorChecks{
		Port80Available: func() bool { return true },
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ok environment WSL") ||
		!strings.Contains(out.String(), "ok windows service available") {
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
	if err := DoctorWithChecks(t.Context(), &out, t.TempDir(), router.NewMemoryStore(), fakeAdminClient{}, DoctorChecks{
		Port80Available: func() bool { return true },
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "fail windows service") ||
		!strings.Contains(out.String(), "connection refused") ||
		!strings.Contains(out.String(), "Run gohere setup from this WSL shell") {
		t.Fatalf("doctor output = %q", out.String())
	}
}

func TestDoctorDoesNotReadWindowsTokenFromWSL(t *testing.T) {
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:         true,
		windowsBinary: true,
	})
	defer restore()

	var out strings.Builder
	if err := DoctorWithChecks(t.Context(), &out, t.TempDir(), router.NewMemoryStore(), fakeAdminClient{}, DoctorChecks{
		Port80Available: func() bool { return true },
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ok windows service available") ||
		strings.Contains(out.String(), "windows service token") {
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
	if err := DoctorWithChecks(t.Context(), &out, t.TempDir(), router.NewMemoryStore(), fakeAdminClient{}, DoctorChecks{
		Port80Available: func() bool { return true },
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "fail windows service unavailable") ||
		!strings.Contains(out.String(), "unauthorized") {
		t.Fatalf("doctor output = %q", out.String())
	}
}

func TestDoctorReportsWindowsServiceRouteErrorFromWSL(t *testing.T) {
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:         true,
		token:         "windows-token",
		windowsBinary: true,
		admin:         routesErrorAdminClient{err: errors.New("network timeout")},
	})
	defer restore()

	var out strings.Builder
	if err := DoctorWithChecks(t.Context(), &out, t.TempDir(), router.NewMemoryStore(), fakeAdminClient{}, DoctorChecks{
		Port80Available: func() bool { return true },
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "fail windows service unavailable") ||
		!strings.Contains(out.String(), "network timeout") {
		t.Fatalf("doctor output = %q", out.String())
	}
	if strings.Contains(out.String(), "auth failed") {
		t.Fatalf("doctor should not report auth failure for network errors: %q", out.String())
	}
}

func TestDoctorIgnoresStaleProfileTokenWhenCompanionWorks(t *testing.T) {
	restore := stubBridgeDetection(t, bridgeStub{
		isWSL:     true,
		token:     "windows-token",
		healthErr: nil,
		admin:     &recordingAdminClient{},
	})
	defer restore()

	var out strings.Builder
	if err := DoctorWithChecks(t.Context(), &out, t.TempDir(), router.NewMemoryStore(), fakeAdminClient{}, DoctorChecks{
		Port80Available: func() bool { return true },
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ok environment WSL") ||
		!strings.Contains(out.String(), "ok windows service available") {
		t.Fatalf("doctor output = %q", out.String())
	}
}

func TestDoctorDoesNotPanicWhenAdminClientCannotBeCreated(t *testing.T) {
	oldDefaultAdminClient := defaultAdminClientFunc
	restoreLocalhostHTTP := stubLocalhostHTTPStatus(t, LocalhostHTTPStatus{OK: false, Detail: "unreachable"})
	defer func() {
		defaultAdminClientFunc = oldDefaultAdminClient
		restoreLocalhostHTTP()
	}()

	defaultAdminClientFunc = func() (adminClient, error) {
		var client *admin.Client
		return client, errors.New("token unavailable")
	}
	var out strings.Builder

	if err := Doctor(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorWithChecksUsesProvidedContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out strings.Builder

	if err := DoctorWithChecks(ctx, &out, t.TempDir(), router.NewMemoryStore(), contextHealthAdminClient{}, DoctorChecks{
		Port80Available: func() bool { return true },
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "fail admin API health") {
		t.Fatalf("doctor output = %q", out.String())
	}
}

func TestDoctorWithStoreReportsPort80Availability(t *testing.T) {
	stateDir := t.TempDir()
	store := router.NewMemoryStore()
	var out strings.Builder

	if err := DoctorWithChecks(t.Context(), &out, stateDir, store, fakeAdminClient{}, DoctorChecks{Port80Available: func() bool {
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

	if err := DoctorWithChecks(t.Context(), &out, stateDir, store, nil, DoctorChecks{Port80Available: func() bool {
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

	if err := DoctorWithChecks(t.Context(), &out, stateDir, store, nil, DoctorChecks{Port80Status: func() Port80Status {
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

	if err := DoctorWithChecks(t.Context(), &out, stateDir, store, nil, DoctorChecks{Port80Status: func() Port80Status {
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

	if err := DoctorWithChecks(t.Context(), &out, t.TempDir(), router.NewMemoryStore(), fakeAdminClient{}, DoctorChecks{Port80Available: func() bool {
		return false
	}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ok port 80 used by gohere service") {
		t.Fatalf("doctor output = %q", out.String())
	}
}

func TestDoctorWithStoreReportsSetcapStatus(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("setcap is Linux-specific")
	}
	stateDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stateDir, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	binaryPath := filepath.Join(stateDir, "bin", "gohere")
	if err := os.WriteFile(binaryPath, []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder

	if err := DoctorWithChecks(t.Context(), &out, stateDir, router.NewMemoryStore(), fakeAdminClient{}, DoctorChecks{
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

	if err := DoctorWithChecks(t.Context(), &out, stateDir, router.NewMemoryStore(), fakeAdminClient{}, DoctorChecks{
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
	if runtime.GOOS != "linux" {
		t.Skip("systemd is Linux-specific")
	}
	var out strings.Builder

	if err := DoctorWithChecks(t.Context(), &out, t.TempDir(), router.NewMemoryStore(), fakeAdminClient{}, DoctorChecks{
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

func (fakeAdminClient) ReserveRoutes(_ context.Context, request router.ReservationRequest) (router.ReservationResult, error) {
	return fakeReservationResult(request), nil
}
func (fakeAdminClient) ActivateRoutes(_ context.Context, _ string, refs []router.RouteRef) ([]router.Route, error) {
	return fakeActivatedRoutes(refs), nil
}
func (fakeAdminClient) ReleaseRoutes(context.Context, string, []router.RouteRef) error { return nil }
func (fakeAdminClient) RenewRoutes(context.Context, string, []router.RouteRef) error   { return nil }
func (fakeAdminClient) DeleteRouteRef(context.Context, router.RouteRef) error          { return nil }

type contextHealthAdminClient struct{}

func (contextHealthAdminClient) Health(ctx context.Context) error {
	return ctx.Err()
}

func (contextHealthAdminClient) Routes(context.Context) ([]router.Route, error) {
	return nil, nil
}

func (contextHealthAdminClient) UpsertRoute(context.Context, router.Route) error {
	return nil
}

func (contextHealthAdminClient) DeleteRoute(context.Context, string) error {
	return nil
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

func (staleTokenAdminClient) ReserveRoutes(context.Context, router.ReservationRequest) (router.ReservationResult, error) {
	return router.ReservationResult{}, admin.ErrUnauthorized
}
func (staleTokenAdminClient) ActivateRoutes(context.Context, string, []router.RouteRef) ([]router.Route, error) {
	return nil, admin.ErrUnauthorized
}
func (staleTokenAdminClient) ReleaseRoutes(context.Context, string, []router.RouteRef) error {
	return admin.ErrUnauthorized
}
func (staleTokenAdminClient) RenewRoutes(context.Context, string, []router.RouteRef) error {
	return admin.ErrUnauthorized
}
func (staleTokenAdminClient) DeleteRouteRef(context.Context, router.RouteRef) error {
	return admin.ErrUnauthorized
}

type unauthorizedBridgeAdminClient struct {
	staleTokenAdminClient
}

func (unauthorizedBridgeAdminClient) ProbeTarget(context.Context, string) (bool, error) {
	return false, admin.ErrUnauthorized
}

type routesErrorAdminClient struct {
	err error
}

func (routesErrorAdminClient) Health(context.Context) error {
	return nil
}

func (c routesErrorAdminClient) Routes(context.Context) ([]router.Route, error) {
	return nil, c.err
}

func (routesErrorAdminClient) UpsertRoute(context.Context, router.Route) error {
	return nil
}

func (routesErrorAdminClient) DeleteRoute(context.Context, string) error {
	return nil
}

func (routesErrorAdminClient) ProbeTarget(context.Context, string) (bool, error) {
	return false, nil
}

type recordingAdminClient struct {
	mu                  sync.Mutex
	upserted            chan struct{}
	upsertedClosed      bool
	route               router.Route
	routes              []router.Route
	statuses            []router.RouteStatus
	statusErr           error
	probeReachable      map[string]bool
	probeCalls          int
	probeReachableAfter int
	probeErr            error
	activateErr         error
	reserveCalls        int
	activateCalls       int
	releaseCalls        int
	deleted             string
	deleteErr           error
}

type routeStatusesUnsupportedClient struct {
	routes []router.Route
}

func (c routeStatusesUnsupportedClient) Health(context.Context) error {
	return nil
}

func (c routeStatusesUnsupportedClient) Routes(context.Context) ([]router.Route, error) {
	return append([]router.Route(nil), c.routes...), nil
}

func (c routeStatusesUnsupportedClient) RouteStatuses(context.Context) ([]router.RouteStatus, error) {
	return nil, admin.ErrRouteStatusesUnsupported
}

func (c routeStatusesUnsupportedClient) UpsertRoute(context.Context, router.Route) error {
	return nil
}

func (c routeStatusesUnsupportedClient) DeleteRoute(context.Context, string) error {
	return nil
}

type multiRecordingAdminClient struct {
	mu              sync.Mutex
	routes          []router.Route
	activated       []router.Route
	deleted         []string
	deleteErr       error
	activateErr     error
	reserveCalls    int
	reservedBatch   int
	activationCalls int
	releaseCalls    int
	probeCalls      map[string]int
}

type cleanupVerifiedAdminClient struct {
	route                  router.Route
	deleteErr              error
	routesAfterDelete      []router.Route
	removeAfterRouteChecks int
	deleteCalled           bool
	routesChecked          bool
	routeChecks            int
}

func (c *multiRecordingAdminClient) Health(context.Context) error {
	return nil
}

func (c *multiRecordingAdminClient) Routes(context.Context) ([]router.Route, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]router.Route(nil), c.routes...), nil
}

func (c *multiRecordingAdminClient) UpsertRoute(ctx context.Context, route router.Route) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.routes = append(c.routes, route)
	return nil
}

func (c *multiRecordingAdminClient) DeleteRoute(ctx context.Context, host string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deleted = append(c.deleted, host)
	return c.deleteErr
}

func (c *multiRecordingAdminClient) ProbeTarget(_ context.Context, target string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.probeCalls == nil {
		c.probeCalls = map[string]int{}
	}
	c.probeCalls[target]++
	return true, nil
}

func (c *multiRecordingAdminClient) ReserveRoutes(_ context.Context, request router.ReservationRequest) (router.ReservationResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reserveCalls++
	c.reservedBatch = len(request.Routes)
	store := &runtimeTestStore{routes: append([]router.Route(nil), c.routes...)}
	result, err := router.ReserveRoutes(store, request, time.Now().UTC())
	c.routes = store.routes
	return result, err
}
func (c *multiRecordingAdminClient) ActivateRoutes(_ context.Context, _ string, refs []router.RouteRef) ([]router.Route, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.activationCalls++
	if c.activateErr != nil {
		return nil, c.activateErr
	}
	var active []router.Route
	for i := range c.routes {
		for _, ref := range refs {
			if c.routes[i].Ref() == ref {
				c.routes[i].State = router.RouteStateActive
				c.routes[i].Target = c.routes[i].PendingTarget
				c.routes[i].PendingTarget = ""
				active = append(active, c.routes[i])
			}
		}
	}
	c.activated = append(c.activated, active...)
	return active, nil
}
func (c *multiRecordingAdminClient) ReleaseRoutes(_ context.Context, _ string, refs []router.RouteRef) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.releaseCalls++
	remove := map[router.RouteRef]bool{}
	for _, ref := range refs {
		remove[ref] = true
	}
	next := c.routes[:0]
	for _, route := range c.routes {
		if !remove[route.Ref()] {
			next = append(next, route)
		}
	}
	c.routes = next
	return nil
}
func (c *multiRecordingAdminClient) RenewRoutes(context.Context, string, []router.RouteRef) error {
	return nil
}
func (c *multiRecordingAdminClient) DeleteRouteRef(context.Context, router.RouteRef) error {
	return nil
}

func (c *multiRecordingAdminClient) upsertedHosts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	routes := c.routes
	if len(c.activated) > 0 {
		routes = c.activated
	}
	hosts := make([]string, 0, len(routes))
	for _, route := range routes {
		hosts = append(hosts, route.Host)
	}
	return hosts
}

func (c *multiRecordingAdminClient) upsertedRoutes() []router.Route {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.activated) > 0 {
		return append([]router.Route(nil), c.activated...)
	}
	return append([]router.Route(nil), c.routes...)
}

func (c *multiRecordingAdminClient) deletedHosts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.deleted...)
}

func (c *cleanupVerifiedAdminClient) Health(context.Context) error {
	return nil
}

func (c *cleanupVerifiedAdminClient) Routes(context.Context) ([]router.Route, error) {
	if c.deleteCalled {
		c.routesChecked = true
		c.routeChecks++
		if c.removeAfterRouteChecks > 0 && c.routeChecks >= c.removeAfterRouteChecks {
			return nil, nil
		}
		return append([]router.Route(nil), c.routesAfterDelete...), nil
	}
	if c.route.Host == "" {
		return nil, nil
	}
	return []router.Route{c.route}, nil
}

func (c *cleanupVerifiedAdminClient) UpsertRoute(ctx context.Context, route router.Route) error {
	c.route = route
	return nil
}

func (c *cleanupVerifiedAdminClient) DeleteRoute(context.Context, string) error {
	c.deleteCalled = true
	return c.deleteErr
}

func (c *recordingAdminClient) Health(context.Context) error {
	return nil
}

func (c *recordingAdminClient) Routes(context.Context) ([]router.Route, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.routes, nil
}

func (c *recordingAdminClient) RouteStatuses(context.Context) ([]router.RouteStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.statusErr != nil {
		return nil, c.statusErr
	}
	if c.statuses != nil {
		return append([]router.RouteStatus(nil), c.statuses...), nil
	}
	statuses := make([]router.RouteStatus, 0, len(c.routes))
	for _, route := range c.routes {
		statuses = append(statuses, router.RouteStatus{Route: route, Status: "ready"})
	}
	return statuses, nil
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
	if c.deleteErr != nil && c.deleted != "" {
		return c.deleteErr
	}
	c.deleted = host
	return nil
}

func (c *recordingAdminClient) ProbeTarget(_ context.Context, target string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.probeCalls++
	if c.probeErr != nil {
		return false, c.probeErr
	}
	if c.probeReachableAfter > 0 {
		return c.probeCalls >= c.probeReachableAfter, nil
	}
	if c.probeReachable != nil {
		return c.probeReachable[target], nil
	}
	return true, nil
}

func (c *recordingAdminClient) ReserveRoutes(_ context.Context, request router.ReservationRequest) (router.ReservationResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reserveCalls++
	stored := append([]router.Route(nil), c.routes...)
	if len(stored) == 0 {
		for _, status := range c.statuses {
			stored = append(stored, status.Route)
		}
	}
	store := &runtimeTestStore{routes: stored}
	result, err := router.ReserveRoutes(store, request, time.Now().UTC())
	c.routes = store.routes
	return result, err
}

func (c *recordingAdminClient) ActivateRoutes(_ context.Context, runID string, refs []router.RouteRef) ([]router.Route, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.activateCalls++
	if c.activateErr != nil {
		return nil, c.activateErr
	}
	store := &runtimeTestStore{routes: append([]router.Route(nil), c.routes...)}
	routes, err := router.ActivateRoutes(store, runID, refs, time.Now().UTC(), router.DefaultRouteLeaseTTL)
	c.routes = store.routes
	if err == nil && len(routes) > 0 {
		c.route = routes[0]
		if c.upserted == nil {
			c.upserted = make(chan struct{})
		}
		if !c.upsertedClosed {
			close(c.upserted)
			c.upsertedClosed = true
		}
	}
	return routes, err
}

func (c *recordingAdminClient) ReleaseRoutes(_ context.Context, runID string, refs []router.RouteRef) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.releaseCalls++
	store := &runtimeTestStore{routes: append([]router.Route(nil), c.routes...)}
	err := router.ReleaseRoutes(store, runID, refs)
	c.routes = store.routes
	return err
}
func (c *recordingAdminClient) RenewRoutes(context.Context, string, []router.RouteRef) error {
	return nil
}
func (c *recordingAdminClient) DeleteRouteRef(_ context.Context, ref router.RouteRef) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	store := &runtimeTestStore{routes: append([]router.Route(nil), c.routes...)}
	err := router.DeleteRouteRef(store, ref)
	c.routes = store.routes
	return err
}

type runtimeTestStore struct{ routes []router.Route }

func (s *runtimeTestStore) Load() ([]router.Route, error) {
	return append([]router.Route(nil), s.routes...), nil
}
func (s *runtimeTestStore) Save(routes []router.Route) error {
	s.routes = append([]router.Route(nil), routes...)
	return nil
}

func fakeReservationResult(request router.ReservationRequest) router.ReservationResult {
	store := &runtimeTestStore{}
	result, _ := router.ReserveRoutes(store, request, time.Now().UTC())
	return result
}
func fakeActivatedRoutes(refs []router.RouteRef) []router.Route {
	routes := make([]router.Route, len(refs))
	for i, ref := range refs {
		routes[i] = router.Route{ID: ref.ID, Generation: ref.Generation, State: router.RouteStateActive}
	}
	return routes
}

type recordingProbeClient struct {
	calls int
}

func (c *recordingProbeClient) ProbeTarget(context.Context, string) (bool, error) {
	c.calls++
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
	isWSL                    bool
	healthErr                error
	token                    string
	tokenErr                 error
	windowsBinary            bool
	wslIP                    string
	reachable                bool
	probeErr                 error
	probeReachable           map[string]bool
	probeHosts               *[]string
	admin                    bridgeAdminClient
	localAdmin               adminClient
	startWindowsErr          error
	startWindowsCalls        *int
	startWindowsMakesHealthy bool
}

func stubBridgeDetection(t *testing.T, stub bridgeStub) func() {
	t.Helper()
	oldDetectWSL := detectWSLFunc
	oldDetectWSL2 := detectWSL2Func
	oldCurrentWSLIP := currentWSLIPFunc
	oldProbeBridge := probeBridgeFunc
	oldDefaultAdminClient := defaultAdminClientFunc
	oldNewWindowsCompanionControl := newWindowsCompanionControlFunc
	oldEnsureWSLRunIdentity := ensureWSLRunIdentityFunc
	oldCurrentWSLMetadata := currentWSLMetadataFunc
	oldEnsureWSLPublicTransport := ensureWSLPublicTransportFunc
	windowsStarted := false

	detectWSLFunc = func() bool {
		return stub.isWSL
	}
	detectWSL2Func = func() bool { return true }
	health := func(context.Context) error {
		if windowsStarted && stub.startWindowsMakesHealthy {
			return nil
		}
		return stub.healthErr
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
	ensure := func(context.Context) error {
		if stub.startWindowsCalls != nil {
			(*stub.startWindowsCalls)++
		}
		if stub.startWindowsErr != nil {
			return stub.startWindowsErr
		}
		windowsStarted = true
		return nil
	}
	companionAdmin := adminClient(fakeAdminClient{})
	if stub.admin != nil {
		companionAdmin = stub.admin
	}
	newWindowsCompanionControlFunc = func(context.Context) (windowsCompanionControl, error) {
		return &testWindowsCompanionControl{
			admin:     companionAdmin,
			installed: true,
			health:    health,
			ensure:    ensure,
		}, nil
	}
	ensureWSLRunIdentityFunc = func(context.Context, companion.Info, io.Writer) (wslRunIdentity, error) {
		return wslRunIdentity{
			OwnerInstance: "test-owner",
			Distro:        "Ubuntu",
			LinuxUser:     "alice",
			RunnerID:      "test-runner",
		}, nil
	}
	currentWSLMetadataFunc = func() (wslIntegrationMetadata, error) {
		return wslIntegrationMetadata{
			OwnerInstance: "test-owner",
			Distro:        "Ubuntu",
			LinuxUser:     "alice",
		}, nil
	}
	ensureWSLPublicTransportFunc = func(context.Context, companion.Info, string) (wslPublicTransport, error) {
		return wslTransportDirect, nil
	}

	return func() {
		detectWSLFunc = oldDetectWSL
		detectWSL2Func = oldDetectWSL2
		currentWSLIPFunc = oldCurrentWSLIP
		probeBridgeFunc = oldProbeBridge
		defaultAdminClientFunc = oldDefaultAdminClient
		newWindowsCompanionControlFunc = oldNewWindowsCompanionControl
		ensureWSLRunIdentityFunc = oldEnsureWSLRunIdentity
		currentWSLMetadataFunc = oldCurrentWSLMetadata
		ensureWSLPublicTransportFunc = oldEnsureWSLPublicTransport
	}
}

func stubCurrentWSLMetadata(t *testing.T) func() {
	t.Helper()
	old := currentWSLMetadataFunc
	currentWSLMetadataFunc = func() (wslIntegrationMetadata, error) {
		return wslIntegrationMetadata{
			OwnerInstance: "test-owner",
			Distro:        "Ubuntu",
			LinuxUser:     "alice",
		}, nil
	}
	return func() { currentWSLMetadataFunc = old }
}

type testWindowsCompanionControl struct {
	admin     adminClient
	installed bool
	health    func(context.Context) error
	ensure    func(context.Context) error
}

func (*testWindowsCompanionControl) Executable() string { return `C:\Temp\gohere.exe` }

func (c *testWindowsCompanionControl) Info(ctx context.Context) (companion.Info, error) {
	routerReady := c.health == nil || c.health(ctx) == nil
	routerInstanceID := ""
	if routerReady {
		routerInstanceID = "test-router"
	}
	return companion.Info{
		CompanionVersion: "test",
		Platform:         "windows",
		Architecture:     "amd64",
		User:             `DESKTOP\Alice`,
		UserProfile:      `C:\Users\Alice`,
		StateDir:         `C:\Users\Alice\.gohere`,
		CAFingerprint:    "test-ca",
		RouterReady:      routerReady,
		RouterInstalled:  c.installed,
		RouterInstanceID: routerInstanceID,
		Capabilities: []string{
			"control.bootstrap",
			"control.ca-certificate",
			"control.delete-route",
			"control.doctor",
			"control.ensure-router",
			"control.health",
			"control.info",
			"control.ready-info",
			"control.probe-target",
			"control.route-statuses",
			"control.routes",
			"control.upsert-route",
			companion.CapabilityReserveRoutes,
			companion.CapabilityActivateRoutes,
			companion.CapabilityReleaseRoutes,
			companion.CapabilityRenewRoutes,
			companion.CapabilityDeleteRouteRef,
			"control.uninstall",
			"control.stop-router",
		},
	}, nil
}

func (c *testWindowsCompanionControl) ReadyInfo(ctx context.Context) (companion.Info, error) {
	info, err := c.Info(ctx)
	if err != nil || !info.RouterInstalled || info.RouterReady {
		return info, err
	}
	if err := c.EnsureRouter(ctx); err != nil {
		return companion.Info{}, err
	}
	info, err = c.Info(ctx)
	if err != nil {
		return companion.Info{}, err
	}
	if !info.RouterReady {
		if c.health != nil {
			if err := c.health(ctx); err != nil {
				return companion.Info{}, err
			}
		}
		return companion.Info{}, errors.New("Windows router is not ready")
	}
	return info, nil
}

func (*testWindowsCompanionControl) Bootstrap(context.Context, bool) (string, error) {
	return "", nil
}

func (*testWindowsCompanionControl) CACertificate(context.Context) (string, error) {
	return "test certificate", nil
}

func (c *testWindowsCompanionControl) EnsureRouter(ctx context.Context) error {
	if c.ensure == nil {
		return nil
	}
	return c.ensure(ctx)
}

func (c *testWindowsCompanionControl) Health(ctx context.Context) error {
	if c.health != nil {
		return c.health(ctx)
	}
	return c.admin.Health(ctx)
}

func (c *testWindowsCompanionControl) Routes(ctx context.Context) ([]router.Route, error) {
	return c.admin.Routes(ctx)
}

func (c *testWindowsCompanionControl) RouteStatuses(ctx context.Context) ([]router.RouteStatus, error) {
	statuses, err := adminRouteStatuses(ctx, c.admin)
	if err != nil {
		return nil, err
	}
	converted := make([]router.RouteStatus, 0, len(statuses))
	for _, status := range statuses {
		converted = append(converted, router.RouteStatus{
			Route:  status.Route,
			Status: string(status.Status),
		})
	}
	return converted, nil
}

func (*testWindowsCompanionControl) Doctor(context.Context) (string, error) {
	return "ok Windows companion\n", nil
}

func (*testWindowsCompanionControl) Uninstall(context.Context, bool) (string, error) {
	return "", nil
}

func (*testWindowsCompanionControl) StopRouter(context.Context) (string, error) {
	return "gohere service stopped.\n", nil
}

func (c *testWindowsCompanionControl) UpsertRoute(ctx context.Context, route router.Route) error {
	return c.admin.UpsertRoute(ctx, route)
}

func (c *testWindowsCompanionControl) DeleteRoute(ctx context.Context, host string) error {
	return c.admin.DeleteRoute(ctx, host)
}

func (c *testWindowsCompanionControl) ProbeTarget(ctx context.Context, target string) (bool, error) {
	probe, ok := c.admin.(bridgeProbeClient)
	if !ok {
		return false, errors.New("test companion does not support target probes")
	}
	return probe.ProbeTarget(ctx, target)
}

func (c *testWindowsCompanionControl) ReserveRoutes(ctx context.Context, request router.ReservationRequest) (router.ReservationResult, error) {
	return c.admin.(routeLifecycleClient).ReserveRoutes(ctx, request)
}
func (c *testWindowsCompanionControl) ActivateRoutes(ctx context.Context, runID string, refs []router.RouteRef) ([]router.Route, error) {
	return c.admin.(routeLifecycleClient).ActivateRoutes(ctx, runID, refs)
}
func (c *testWindowsCompanionControl) ReleaseRoutes(ctx context.Context, runID string, refs []router.RouteRef) error {
	return c.admin.(routeLifecycleClient).ReleaseRoutes(ctx, runID, refs)
}
func (c *testWindowsCompanionControl) RenewRoutes(ctx context.Context, runID string, refs []router.RouteRef) error {
	return c.admin.(routeLifecycleClient).RenewRoutes(ctx, runID, refs)
}
func (c *testWindowsCompanionControl) DeleteRouteRef(ctx context.Context, ref router.RouteRef) error {
	return c.admin.(routeLifecycleClient).DeleteRouteRef(ctx, ref)
}

func stubLocalhostHTTPStatus(t *testing.T, status LocalhostHTTPStatus) func() {
	t.Helper()
	oldLocalhostHTTPStatus := localhostHTTPStatusFunc
	localhostHTTPStatusFunc = func(context.Context) LocalhostHTTPStatus {
		return status
	}
	return func() {
		localhostHTTPStatusFunc = oldLocalhostHTTPStatus
	}
}

func runFakeWindowsNPM() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "47654"
	}
	fmt.Fprintf(os.Stdout, "Local: http://127.0.0.1:%s\n", port)
	time.Sleep(time.Second)
}

func installFakeWindowsNPM(t *testing.T, output string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	outputFile, err := os.OpenFile(output, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(outputFile, input); err != nil {
		outputFile.Close()
		t.Fatal(err)
	}
	if err := outputFile.Close(); err != nil {
		t.Fatal(err)
	}
}

func windowsLongPathEndingWith(t *testing.T, last string, minLength int) string {
	t.Helper()
	root := t.TempDir()
	entries := []string{}
	for i := 0; ; i++ {
		name := fmt.Sprintf("missing-%03d-%s", i, strings.Repeat("x", 64))
		entries = append(entries, filepath.Join(root, name))
		path := strings.Join(append(append([]string(nil), entries...), last), string(os.PathListSeparator))
		if len(path) >= minLength {
			return path
		}
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
	got, ok := envValue(env, key)
	if !ok {
		t.Fatalf("missing env %s", key)
	}
	if got != want {
		t.Fatalf("%s = %q, want %q", key, got, want)
	}
}

func assertMissingEnv(t *testing.T, env []string, key string) {
	t.Helper()
	if got, ok := envValue(env, key); ok {
		t.Fatalf("%s = %q, want missing", key, got)
	}
}

func mustEnv(t *testing.T, env []string, key string) string {
	t.Helper()
	got, ok := envValue(env, key)
	if !ok {
		t.Fatalf("missing env %s", key)
	}
	return got
}

func envValue(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, item := range env {
		if len(item) >= len(prefix) && item[:len(prefix)] == prefix {
			return item[len(prefix):], true
		}
	}
	return "", false
}

func appHelperCommand(command string) []string {
	return []string{os.Args[0], "-test.run=TestAppCommandHelper", "--", command}
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

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func countString(values []string, want string) int {
	count := 0
	for _, value := range values {
		if value == want {
			count++
		}
	}
	return count
}

type interleavingAppRouteStore struct {
	routes     []router.Route
	added      router.Route
	injected   bool
	usedUpdate bool
}

func (s *interleavingAppRouteStore) Load() ([]router.Route, error) {
	routes := cloneAppTestRoutes(s.routes)
	if !s.injected {
		s.injected = true
		s.routes = append(s.routes, s.added)
	}
	return routes, nil
}

func (s *interleavingAppRouteStore) Save(routes []router.Route) error {
	s.routes = cloneAppTestRoutes(routes)
	return nil
}

func (s *interleavingAppRouteStore) Update(fn func([]router.Route) ([]router.Route, error)) error {
	s.usedUpdate = true
	next, err := fn(cloneAppTestRoutes(s.routes))
	if err != nil {
		return err
	}
	s.routes = cloneAppTestRoutes(next)
	return nil
}

func cloneAppTestRoutes(routes []router.Route) []router.Route {
	return append([]router.Route(nil), routes...)
}

type brokenRouteStore struct {
	err error
}

func (s brokenRouteStore) Load() ([]router.Route, error) {
	return nil, s.err
}

func (s brokenRouteStore) Save([]router.Route) error {
	return nil
}

type failingPromptReader struct{}

func (failingPromptReader) Read([]byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}

func TestAppCommandHelper(t *testing.T) {
	if os.Getenv("GOHERE_APP_HELPER_PROCESS") != "1" {
		return
	}

	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) < 2 {
		os.Exit(2)
	}

	switch args[1] {
	case "print-port-fail":
		printAppHelperLocalURL()
		time.Sleep(500 * time.Millisecond)
		os.Exit(1)
	case "print-port-sleep":
		printAppHelperLocalURL()
		time.Sleep(10 * time.Second)
	default:
		os.Exit(2)
	}
	os.Exit(0)
}

func printAppHelperLocalURL() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "47654"
	}
	fmt.Fprintf(os.Stdout, "Local: http://127.0.0.1:%s\n", port)
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
