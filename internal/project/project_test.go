package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeHostnameName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"lowercase", "MyProject", "myproject"},
		{"separators become hyphens", "my_project.site name", "my-project-site-name"},
		{"invalid characters stripped", "@scope/my app!", "scopemy-app"},
		{"non ascii stripped", "mañana café", "maana-caf"},
		{"repeated hyphens collapse", "my---app", "my-app"},
		{"empty fallback", "!!!", "app"},
		{"long label truncated", "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijkl", "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijk"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeHostnameName(tt.in); got != tt.want {
				t.Fatalf("NormalizeHostnameName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestPackageManagerDetection(t *testing.T) {
	tests := []struct {
		name      string
		files     map[string]string
		want      PackageManager
		wantFound bool
	}{
		{"pnpm lockfile wins", map[string]string{"pnpm-lock.yaml": "", "package.json": `{"packageManager":"npm@10.0.0"}`}, PackageManagerPNPM, true},
		{"npm lockfile", map[string]string{"package-lock.json": ""}, PackageManagerNPM, true},
		{"yarn lockfile", map[string]string{"yarn.lock": ""}, PackageManagerYarn, true},
		{"bun lockfile", map[string]string{"bun.lock": ""}, PackageManagerBun, true},
		{"packageManager field", map[string]string{"package.json": `{"packageManager":"pnpm@9.0.0"}`}, PackageManagerPNPM, true},
		{"default npm", map[string]string{"package.json": `{}`}, PackageManagerNPM, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := tempProject(t, tt.files)
			got, found, err := DetectPackageManager(dir)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want || found != tt.wantFound {
				t.Fatalf("DetectPackageManager() = %q, %v; want %q, %v", got, found, tt.want, tt.wantFound)
			}
		})
	}
}

func TestFindNearestPackageJSON(t *testing.T) {
	root := tempProject(t, map[string]string{
		"package.json":          `{"name":"root"}`,
		"apps/web/package.json": `{"name":"web"}`,
	})
	if err := os.MkdirAll(filepath.Join(root, "apps", "web", "src"), 0755); err != nil {
		t.Fatal(err)
	}

	got, found, err := FindNearestPackageJSON(filepath.Join(root, "apps", "web", "src"))
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected package.json to be found")
	}
	want := filepath.Join(root, "apps", "web", "package.json")
	if got != want {
		t.Fatalf("nearest package.json = %q, want %q", got, want)
	}
}

func TestFindNearestPackageJSONStopsAtGitRoot(t *testing.T) {
	root := tempProject(t, map[string]string{
		"package.json": `{"name":"outside"}`,
		"repo/.git":    "",
	})
	dir := filepath.Join(root, "repo", "site")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}

	_, found, err := FindNearestPackageJSON(dir)
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("package search should not climb past git root")
	}
}

func TestFindNearestPackageJSONIncludesGitRoot(t *testing.T) {
	root := tempProject(t, map[string]string{
		"repo/.git":         "",
		"repo/package.json": `{"name":"repo"}`,
	})
	dir := filepath.Join(root, "repo", "site")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}

	got, found, err := FindNearestPackageJSON(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected package.json at git root")
	}
	want := filepath.Join(root, "repo", "package.json")
	if got != want {
		t.Fatalf("package.json = %q, want %q", got, want)
	}
}

func TestScriptLookup(t *testing.T) {
	dir := tempProject(t, map[string]string{
		"package.json": `{"scripts":{"dev":"vite","dev:web":"vite --host 0.0.0.0"}}`,
	})

	pkg, err := ReadPackageJSON(filepath.Join(dir, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, ok := pkg.Script("dev:web")
	if !ok {
		t.Fatal("expected dev:web script")
	}
	if got != "vite --host 0.0.0.0" {
		t.Fatalf("script = %q", got)
	}

	available := pkg.AvailableScripts()
	if len(available) != 2 || available[0] != "dev" || available[1] != "dev:web" {
		t.Fatalf("available scripts = %#v", available)
	}
}

func TestProjectHostname(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		cwd   string
		want  string
	}{
		{
			name:  "standalone uses package name",
			files: map[string]string{"package.json": `{"name":"@scope/My_App"}`},
			cwd:   ".",
			want:  "my-app.localhost",
		},
		{
			name: "workspace package uses package and root",
			files: map[string]string{
				"package.json":          `{"name":"eventca","workspaces":["apps/*"]}`,
				"apps/web/package.json": `{"name":"web"}`,
			},
			cwd:  "apps/web",
			want: "web.eventca.localhost",
		},
		{
			name: "workspace root stays standalone",
			files: map[string]string{
				"package.json": `{"name":"eventca","workspaces":["apps/*"]}`,
			},
			cwd:  ".",
			want: "eventca.localhost",
		},
		{
			name: "folder fallback",
			files: map[string]string{
				"my page/package.json": `{}`,
			},
			cwd:  "my page",
			want: "my-page.localhost",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := tempProject(t, tt.files)
			got, err := HostnameForProject(filepath.Join(root, tt.cwd))
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("HostnameForProject() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveHostnameConflict(t *testing.T) {
	active := map[string]string{
		"myproject.localhost":        "/other/myproject",
		"parent-myproject.localhost": "/other/parent-myproject",
	}

	got := ResolveHostnameConflict("myproject.localhost", "/work/parent/myproject", active)
	if got != "parent-myproject-2.localhost" {
		t.Fatalf("ResolveHostnameConflict() = %q", got)
	}

	got = ResolveHostnameConflict("myproject.localhost", "/work/parent/myproject", map[string]string{
		"myproject.localhost": "/work/parent/myproject",
	})
	if got != "myproject.localhost" {
		t.Fatalf("same cwd should reuse hostname, got %q", got)
	}

	got = ResolveHostnameConflict("myproject.localhost", "/work/parent/myproject", map[string]string{
		"MyProject.localhost": "/other/myproject",
	})
	if got != "parent-myproject.localhost" {
		t.Fatalf("case-insensitive conflict = %q, want parent-myproject.localhost", got)
	}

	longParent := strings.Repeat("parent", 12)
	longBase := strings.Repeat("project", 12) + ".localhost"
	got = ResolveHostnameConflict(longBase, filepath.Join("/work", longParent, strings.TrimSuffix(longBase, ".localhost")), map[string]string{
		longBase: "/other/project",
	})
	label := strings.TrimSuffix(got, ".localhost")
	if len(label) > 63 {
		t.Fatalf("conflict label length = %d for %q, want <= 63", len(label), label)
	}

	got = conflictHost(strings.Repeat("p", 40), strings.Repeat("b", 40), 2)
	label = strings.TrimSuffix(got, ".localhost")
	if len(label) > 63 || !strings.HasSuffix(label, "-2") {
		t.Fatalf("suffixed conflict label = %q length %d, want <= 63 and -2 suffix", label, len(label))
	}

	got = conflictHost(strings.Repeat("p", 31), strings.Repeat("b", 31), 0)
	label = strings.TrimSuffix(got, ".localhost")
	if len(label) != 63 {
		t.Fatalf("unsuffixed 63-char conflict label length = %d for %q, want 63", len(label), label)
	}

	got = conflictHost(strings.Repeat("p", 31), strings.Repeat("b", 31), 2)
	label = strings.TrimSuffix(got, ".localhost")
	if len(label) != 63 || !strings.HasSuffix(label, "-2") {
		t.Fatalf("suffixed 63-char conflict label = %q length %d, want 63 and -2 suffix", label, len(label))
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
