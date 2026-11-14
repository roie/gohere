package app

import (
	"reflect"
	"strings"
	"testing"

	"github.com/roie/gohere/internal/cli"
	"github.com/roie/gohere/internal/project"
)

func TestPrepareRunCoordinatesBrowserOpening(t *testing.T) {
	for _, script := range []string{"vite --open /docs --port 3000 --host localhost", "webpack serve --open --port 3000", "node server.js", "nuxt dev", "parcel serve index.html", "webpack build"} {
		t.Run(script, func(t *testing.T) {
			dir := tempProject(t, map[string]string{"package.json": `{"scripts":{"dev":"` + script + `"}}`})
			cmd := cli.Command{Kind: cli.CommandRun, Script: "dev", TargetPort: 3000}
			baseline, err := PrepareRun(cmd, dir)
			if err != nil {
				t.Fatal(err)
			}
			cmd.Open = true
			plan, err := PrepareRun(cmd, dir)
			if err != nil {
				t.Fatal(err)
			}
			want := baseline.Command
			if strings.HasPrefix(script, "vite ") || strings.HasPrefix(script, "webpack serve") {
				want = append(want, "--", "--no-open")
			}
			if !reflect.DeepEqual(plan.Command, want) {
				t.Fatalf("command = %q, want %q", plan.Command, want)
			}
			if !reflect.DeepEqual(plan.Env, baseline.Env) {
				t.Fatal("browser coordination changed child environment")
			}
		})
	}
}

func TestPrepareRawRunCoordinatesBrowserOpening(t *testing.T) {
	for _, open := range []bool{false, true} {
		for _, tool := range []string{"vite", "node"} {
			raw := []string{tool, "--open"}
			plan, err := PrepareRun(cli.Command{Kind: cli.CommandRaw, Raw: raw, Open: open}, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			want := []string{tool, "--open"}
			if open && tool == "vite" {
				want = append(want, "--no-open")
			}
			if !reflect.DeepEqual(plan.Command, want) {
				t.Fatalf("open=%v: command=%q, want %q", open, plan.Command, want)
			}
			if len(raw) != 2 {
				t.Fatal("modified raw command")
			}
		}
	}
}

func TestPrepareWorkspaceRunCoordinatesBrowserOpening(t *testing.T) {
	dir := tempProject(t, map[string]string{"package.json": `{"name":"workspace"}`, "apps/web/package.json": `{"name":"web"}`})
	pkg := project.WorkspacePackage{Dir: dir + "/apps/web", Script: "vite --open --port 3000"}
	for _, test := range []struct {
		pm     project.PackageManager
		prefix []string
	}{
		{project.PackageManagerNPM, []string{"npm", "run", "dev"}},
		{project.PackageManagerPNPM, []string{"pnpm", "run", "dev"}},
		{project.PackageManagerYarn, []string{"yarn", "dev"}},
		{project.PackageManagerBun, []string{"bun", "run", "dev"}},
	} {
		pm := test.pm
		for _, open := range []bool{false, true} {
			plan, err := prepareWorkspacePackageRun(cli.Command{Script: "dev", Open: open}, dir, pm, pkg, "127.0.0.1")
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"--no-open"}
			if pm == project.PackageManagerNPM {
				want = append([]string{"--"}, want...)
			}
			if !open {
				want = nil
			}
			want = append(append([]string(nil), test.prefix...), want...)
			if !reflect.DeepEqual(plan.Command, want) {
				t.Fatalf("%s open=%v: command=%q, want %q", pm, open, plan.Command, want)
			}
		}
	}
}
