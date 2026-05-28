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

func TestDiscoverWorkspacePackagesFromPackageJSONWorkspaces(t *testing.T) {
	root := tempProject(t, map[string]string{
		"package.json":             `{"name":"ctrltube","workspaces":["apps/*","packages/*"],"scripts":{"dev":"pnpm --parallel --filter @ctrltube/worker --filter @ctrltube/web dev"}}`,
		"apps/web/package.json":    `{"name":"@ctrltube/web","scripts":{"dev":"vite"}}`,
		"apps/worker/package.json": `{"name":"@ctrltube/worker","scripts":{"dev":"wrangler dev"}}`,
		"packages/ui/package.json": `{"name":"@ctrltube/ui","scripts":{"build":"tsc"}}`,
		"apps/ignored/readme.md":   "not a package",
	})

	packages, found, err := DiscoverWorkspacePackages(root, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected workspace root")
	}
	if len(packages) != 2 {
		t.Fatalf("packages = %#v, want 2 dev packages", packages)
	}
	if packages[0].Name != "@ctrltube/web" || packages[0].ShortName != "web" || packages[0].Dir != filepath.Join(root, "apps", "web") {
		t.Fatalf("first package = %#v", packages[0])
	}
	if packages[1].Name != "@ctrltube/worker" || packages[1].ShortName != "worker" || packages[1].Dir != filepath.Join(root, "apps", "worker") {
		t.Fatalf("second package = %#v", packages[1])
	}
}

func TestDiscoverWorkspacePackagesFromPackageJSONWorkspacesObject(t *testing.T) {
	root := tempProject(t, map[string]string{
		"package.json":          `{"name":"repo","workspaces":{"packages":["apps/*"]}}`,
		"apps/api/package.json": `{"name":"api","scripts":{"start":"node server.js"}}`,
		"apps/web/package.json": `{"name":"web","scripts":{"dev":"vite","start":"vite --host 0.0.0.0"}}`,
	})

	packages, found, err := DiscoverWorkspacePackages(root, "start")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected workspace root")
	}
	if len(packages) != 2 || packages[0].ShortName != "api" || packages[1].ShortName != "web" {
		t.Fatalf("packages = %#v", packages)
	}
}

func TestDiscoverWorkspacePackagesFromPNPMWorkspace(t *testing.T) {
	root := tempProject(t, map[string]string{
		"package.json":              `{"name":"repo"}`,
		"pnpm-workspace.yaml":       "packages:\n  - 'apps/*'\n  - \"services/*\"\n",
		"apps/web/package.json":     `{"name":"web","scripts":{"dev":"vite"}}`,
		"services/api/package.json": `{"name":"api","scripts":{"dev":"node index.js"}}`,
	})

	packages, found, err := DiscoverWorkspacePackages(root, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected workspace root")
	}
	if len(packages) != 2 || packages[0].ShortName != "web" || packages[1].ShortName != "api" {
		t.Fatalf("packages = %#v", packages)
	}
}

func TestDiscoverWorkspacePackagesFromPNPMWorkspaceInlineListAndComments(t *testing.T) {
	root := tempProject(t, map[string]string{
		"package.json":              `{"name":"repo"}`,
		"pnpm-workspace.yaml":       "packages: ['apps/*', \"services/*\"] # app workspaces\n",
		"apps/web/package.json":     `{"name":"web","scripts":{"dev":"vite"}}`,
		"services/api/package.json": `{"name":"api","scripts":{"dev":"node index.js"}}`,
	})

	packages, found, err := DiscoverWorkspacePackages(root, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected workspace root")
	}
	if len(packages) != 2 || packages[0].ShortName != "web" || packages[1].ShortName != "api" {
		t.Fatalf("packages = %#v", packages)
	}
}

func TestDiscoverWorkspacePackagesSupportsGlobstarPatterns(t *testing.T) {
	root := tempProject(t, map[string]string{
		"package.json":                        `{"name":"repo"}`,
		"pnpm-workspace.yaml":                 "packages:\n  - 'packages/**'\n",
		"packages/tools/api/package.json":     `{"name":"api","scripts":{"dev":"node index.js"}}`,
		"packages/tools/worker/package.json":  `{"name":"worker","scripts":{"dev":"node index.js"}}`,
		"packages/tools/readme.md":            "not a package",
		"packages/tools/ignored/package.json": `{"name":"ignored","scripts":{"build":"tsc"}}`,
	})

	packages, found, err := DiscoverWorkspacePackages(root, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected workspace root")
	}
	if len(packages) != 2 || packages[0].ShortName != "api" || packages[1].ShortName != "worker" {
		t.Fatalf("packages = %#v, want nested api and worker packages", packages)
	}
}

func TestDiscoverWorkspacePackagesExcludesRootPackage(t *testing.T) {
	root := tempProject(t, map[string]string{
		"package.json":          `{"name":"repo","workspaces":[".","apps/*"],"scripts":{"dev":"pnpm --parallel --filter web dev"}}`,
		"apps/web/package.json": `{"name":"web","scripts":{"dev":"vite"}}`,
	})

	packages, found, err := DiscoverWorkspacePackages(root, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected workspace root")
	}
	if len(packages) != 1 || packages[0].ShortName != "web" {
		t.Fatalf("packages = %#v, want only web package", packages)
	}
}

func TestDiscoverWorkspacePackagesAppliesNegatedPatterns(t *testing.T) {
	root := tempProject(t, map[string]string{
		"package.json":             `{"name":"repo","workspaces":["apps/*","!apps/legacy"]}`,
		"apps/web/package.json":    `{"name":"web","scripts":{"dev":"vite"}}`,
		"apps/legacy/package.json": `{"name":"legacy","scripts":{"dev":"vite --legacy"}}`,
	})

	packages, found, err := DiscoverWorkspacePackages(root, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected workspace root")
	}
	if len(packages) != 1 || packages[0].ShortName != "web" {
		t.Fatalf("packages = %#v, want only non-negated web package", packages)
	}
}

func TestDiscoverWorkspacePackagesReportsNonWorkspaceRoot(t *testing.T) {
	root := tempProject(t, map[string]string{
		"package.json": `{"name":"app","scripts":{"dev":"vite"}}`,
	})

	packages, found, err := DiscoverWorkspacePackages(root, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if found || len(packages) != 0 {
		t.Fatalf("packages = %#v, found=%v; want non-workspace", packages, found)
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

func TestResolveHostnameConflictKeepsLabelsWithinDNSLimit(t *testing.T) {
	parent := strings.Repeat("p", 31)
	base := strings.Repeat("b", 31)
	desired := base + ".localhost"
	cwd := filepath.Join("/work", parent, base)

	got := ResolveHostnameConflict(desired, cwd, map[string]string{
		desired: "/other/project",
	})
	label := strings.TrimSuffix(got, ".localhost")
	if len(label) != 63 {
		t.Fatalf("conflict label length = %d for %q, want 63", len(label), label)
	}

	got = ResolveHostnameConflict(desired, cwd, map[string]string{
		desired:                       "/other/project",
		conflictHost(parent, base, 0): "/other/conflict",
	})
	label = strings.TrimSuffix(got, ".localhost")
	if len(label) != 63 || !strings.HasSuffix(label, "-2") {
		t.Fatalf("suffixed conflict label = %q length %d, want 63 and -2 suffix", label, len(label))
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
