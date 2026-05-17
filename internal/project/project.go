package project

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type PackageManager string

const (
	PackageManagerNPM  PackageManager = "npm"
	PackageManagerPNPM PackageManager = "pnpm"
	PackageManagerYarn PackageManager = "yarn"
	PackageManagerBun  PackageManager = "bun"
)

type PackageJSON struct {
	Name           string            `json:"name"`
	PackageManager string            `json:"packageManager"`
	Scripts        map[string]string `json:"scripts"`
	Workspaces     json.RawMessage   `json:"workspaces"`
}

func ReadPackageJSON(path string) (PackageJSON, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return PackageJSON{}, err
	}

	var pkg PackageJSON
	if err := json.Unmarshal(data, &pkg); err != nil {
		return PackageJSON{}, err
	}
	return pkg, nil
}

func (p PackageJSON) Script(name string) (string, bool) {
	script, ok := p.Scripts[name]
	return script, ok
}

func (p PackageJSON) AvailableScripts() []string {
	scripts := make([]string, 0, len(p.Scripts))
	for name := range p.Scripts {
		scripts = append(scripts, name)
	}
	sort.Strings(scripts)
	return scripts
}

func FindNearestPackageJSON(start string) (string, bool, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", false, err
	}

	info, err := os.Stat(dir)
	if err != nil {
		return "", false, err
	}
	if !info.IsDir() {
		dir = filepath.Dir(dir)
	}

	for {
		candidate := filepath.Join(dir, "package.json")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", false, err
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false, nil
		}
		dir = parent
	}
}

func DetectPackageManager(projectDir string) (PackageManager, bool, error) {
	lockfiles := []struct {
		name string
		pm   PackageManager
	}{
		{"pnpm-lock.yaml", PackageManagerPNPM},
		{"package-lock.json", PackageManagerNPM},
		{"yarn.lock", PackageManagerYarn},
		{"bun.lock", PackageManagerBun},
		{"bun.lockb", PackageManagerBun},
	}

	for _, lockfile := range lockfiles {
		if _, err := os.Stat(filepath.Join(projectDir, lockfile.name)); err == nil {
			return lockfile.pm, true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", false, err
		}
	}

	pkg, err := ReadPackageJSON(filepath.Join(projectDir, "package.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}

	if pm := packageManagerFromField(pkg.PackageManager); pm != "" {
		return pm, true, nil
	}
	return PackageManagerNPM, true, nil
}

func packageManagerFromField(value string) PackageManager {
	name, _, _ := strings.Cut(value, "@")
	switch name {
	case "pnpm":
		return PackageManagerPNPM
	case "npm":
		return PackageManagerNPM
	case "yarn":
		return PackageManagerYarn
	case "bun":
		return PackageManagerBun
	default:
		return ""
	}
}

func NormalizeHostnameName(name string) string {
	name = strings.ToLower(name)

	var out strings.Builder
	lastHyphen := false
	for _, r := range name {
		switch {
		case r == ' ' || r == '_' || r == '.':
			if !lastHyphen {
				out.WriteByte('-')
				lastHyphen = true
			}
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-':
			if r == '-' {
				if lastHyphen {
					continue
				}
				lastHyphen = true
			} else {
				lastHyphen = false
			}
			out.WriteRune(r)
		}
	}

	normalized := strings.Trim(out.String(), "-")
	if normalized == "" {
		return "app"
	}
	if len(normalized) > 63 {
		normalized = strings.TrimRight(normalized[:63], "-")
	}
	if normalized == "" {
		return "app"
	}
	return normalized
}

func HostnameForProject(cwd string) (string, error) {
	packagePath, found, err := FindNearestPackageJSON(cwd)
	if err != nil {
		return "", err
	}
	if !found {
		return NormalizeHostnameName(filepath.Base(cwd)) + ".localhost", nil
	}

	projectDir := filepath.Dir(packagePath)
	pkg, err := ReadPackageJSON(packagePath)
	if err != nil {
		return "", err
	}

	projectName := packageNameOrFolder(pkg.Name, projectDir)
	rootDir, rootName, isWorkspaceChild, err := findWorkspaceRoot(projectDir)
	if err != nil {
		return "", err
	}
	if isWorkspaceChild && rootDir != projectDir {
		return NormalizeHostnameName(projectName) + "." + NormalizeHostnameName(rootName) + ".localhost", nil
	}

	return NormalizeHostnameName(projectName) + ".localhost", nil
}

func packageNameOrFolder(name, dir string) string {
	if name == "" {
		return filepath.Base(dir)
	}
	if strings.HasPrefix(name, "@") {
		if _, after, ok := strings.Cut(name, "/"); ok {
			return after
		}
	}
	return name
}

func findWorkspaceRoot(start string) (string, string, bool, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", "", false, err
	}

	for {
		name, ok, err := workspaceRootName(dir)
		if err != nil {
			return "", "", false, err
		}
		if ok {
			return dir, name, true, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", false, nil
		}
		dir = parent
	}
}

func workspaceRootName(dir string) (string, bool, error) {
	for _, marker := range []string{"pnpm-workspace.yaml", "lerna.json", "turbo.json", "nx.json"} {
		if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
			return nameFromPackageOrFolder(dir)
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", false, err
		}
	}

	pkg, err := ReadPackageJSON(filepath.Join(dir, "package.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	if len(pkg.Workspaces) > 0 && string(pkg.Workspaces) != "null" {
		return packageNameOrFolder(pkg.Name, dir), true, nil
	}
	return "", false, nil
}

func nameFromPackageOrFolder(dir string) (string, bool, error) {
	pkg, err := ReadPackageJSON(filepath.Join(dir, "package.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	if err == nil && pkg.Name != "" {
		return packageNameOrFolder(pkg.Name, dir), true, nil
	}
	return filepath.Base(dir), true, nil
}

func ResolveHostnameConflict(desiredHost, cwd string, active map[string]string) string {
	if existingCWD, ok := activeHostCWD(active, desiredHost); !ok || samePath(existingCWD, cwd) {
		return desiredHost
	}

	base := strings.TrimSuffix(desiredHost, ".localhost")
	parent := NormalizeHostnameName(filepath.Base(filepath.Dir(cwd)))
	candidate := conflictHost(parent, base, 0)
	if existingCWD, ok := activeHostCWD(active, candidate); !ok || samePath(existingCWD, cwd) {
		return candidate
	}

	for suffix := 2; ; suffix++ {
		candidate = conflictHost(parent, base, suffix)
		if existingCWD, ok := activeHostCWD(active, candidate); !ok || samePath(existingCWD, cwd) {
			return candidate
		}
	}
}

func conflictHost(parent, base string, suffix int) string {
	label := parent + "-" + strings.TrimSuffix(base, ".localhost")
	if suffix > 0 {
		suffixPart := "-" + strconv.Itoa(suffix)
		label = NormalizeHostnameName(label)
		if len(label)+len(suffixPart) > 63 {
			label = strings.TrimRight(label[:63-len(suffixPart)], "-")
		}
		if label == "" {
			label = "app"
		}
		return label + suffixPart + ".localhost"
	}
	return NormalizeHostnameName(label) + ".localhost"
}

func activeHostCWD(active map[string]string, host string) (string, bool) {
	for activeHost, cwd := range active {
		if strings.EqualFold(activeHost, host) {
			return cwd, true
		}
	}
	return "", false
}

func samePath(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	return errA == nil && errB == nil && absA == absB
}
