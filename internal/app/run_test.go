package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/roie/gohere/internal/cli"
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
	want := []string{"pnpm", "run", "dev", "--", "--host", "127.0.0.1", "--port", itoa(plan.Port), "--strictPort"}
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
