package project

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
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

	maxHostnameConflictAttempts = 100
)

type PackageJSON struct {
	Name           string            `json:"name"`
	PackageManager string            `json:"packageManager"`
	Scripts        map[string]string `json:"scripts"`
	Workspaces     json.RawMessage   `json:"workspaces"`
}

type WorkspacePackage struct {
	Dir         string
	PackagePath string
	Name        string
	ShortName   string
	Script      string
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
		if isGitRoot(dir) || isHomeDir(dir) {
			return "", false, nil
		}
		dir = parent
	}
}

func isGitRoot(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return true
	}
	return false
}

func isHomeDir(dir string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	home, err = filepath.Abs(home)
	if err != nil {
		return false
	}
	return samePath(dir, home)
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
	normalized := normalizeHostnameName(name)
	if normalized == "" {
		return "app"
	}
	return normalized
}

func NormalizeHostnameAlias(name string) (string, bool) {
	normalized := normalizeHostnameName(name)
	return normalized, normalized != ""
}

func normalizeHostnameName(name string) string {
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
	if len(normalized) > 63 {
		normalized = strings.TrimRight(normalized[:63], "-")
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

func ProjectNameForRoot(root string) (string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	pkg, err := ReadPackageJSON(filepath.Join(root, "package.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return NormalizeHostnameName(filepath.Base(root)), nil
		}
		return "", err
	}
	return NormalizeHostnameName(packageNameOrFolder(pkg.Name, root)), nil
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

func DiscoverWorkspacePackages(root, script string) ([]WorkspacePackage, bool, error) {
	packages, found, err := DiscoverWorkspacePackageDirs(root)
	if err != nil || !found {
		return packages, found, err
	}
	if script == "" {
		script = "dev"
	}

	filtered := packages[:0]
	for _, workspacePackage := range packages {
		pkg, err := ReadPackageJSON(workspacePackage.PackagePath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, true, err
		}
		scriptCommand, ok := pkg.Script(script)
		if !ok {
			continue
		}
		workspacePackage.Script = scriptCommand
		filtered = append(filtered, workspacePackage)
	}
	return filtered, true, nil
}

func DiscoverWorkspacePackageDirs(root string) ([]WorkspacePackage, bool, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, false, err
	}

	patterns, found, err := workspacePatterns(root)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}

	rootPackagePath := filepath.Join(root, "package.json")
	seen := map[string]bool{}
	excluded := map[string]bool{}
	dirs := map[string]string{}
	var ordered []string
	var packages []WorkspacePackage
	for _, rawPattern := range patterns {
		pattern := strings.TrimSpace(rawPattern)
		if pattern == "" {
			continue
		}
		negated := strings.HasPrefix(pattern, "!")
		if negated {
			pattern = strings.TrimSpace(strings.TrimPrefix(pattern, "!"))
		}
		if pattern == "" {
			continue
		}
		matches, err := workspacePackageMatches(root, pattern)
		if err != nil {
			return nil, true, err
		}
		for _, match := range matches {
			if samePath(match.PackagePath, rootPackagePath) {
				continue
			}
			if negated {
				excluded[match.PackagePath] = true
				continue
			}
			if seen[match.PackagePath] {
				continue
			}
			seen[match.PackagePath] = true
			dirs[match.PackagePath] = match.Dir
			ordered = append(ordered, match.PackagePath)
		}
	}

	for _, packagePath := range ordered {
		if excluded[packagePath] {
			continue
		}
		dir := dirs[packagePath]
		pkg, err := ReadPackageJSON(packagePath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, true, err
		}
		shortName := packageNameOrFolder(pkg.Name, dir)
		packages = append(packages, WorkspacePackage{
			Dir:         dir,
			PackagePath: packagePath,
			Name:        pkg.Name,
			ShortName:   shortName,
		})
	}
	return packages, true, nil
}

func workspacePackageMatches(root, pattern string) ([]WorkspacePackage, error) {
	if strings.Contains(pattern, "**") {
		return workspacePackageGlobstarMatches(root, pattern)
	}
	matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(pattern)))
	if err != nil {
		return nil, err
	}
	var packages []WorkspacePackage
	for _, match := range matches {
		dir := match
		info, err := os.Stat(match)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			if filepath.Base(match) != "package.json" {
				continue
			}
			dir = filepath.Dir(match)
		}
		packagePath := filepath.Clean(filepath.Join(dir, "package.json"))
		packages = append(packages, WorkspacePackage{
			Dir:         filepath.Clean(dir),
			PackagePath: packagePath,
		})
	}
	return packages, nil
}

func workspacePackageGlobstarMatches(root, pattern string) ([]WorkspacePackage, error) {
	pattern = filepath.ToSlash(filepath.Clean(filepath.FromSlash(pattern)))
	var packages []WorkspacePackage
	err := filepath.WalkDir(root, func(filePath string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if filePath != root && (skipWorkspaceWalkDir(entry.Name()) || skipGeneratedWorkspaceDir(root, filePath, entry.Name())) {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() != "package.json" {
			return nil
		}
		packagePath := filepath.Clean(filePath)
		dir := filepath.Dir(packagePath)
		if !workspacePackageMatchesPattern(root, pattern, dir, packagePath) {
			return nil
		}
		packages = append(packages, WorkspacePackage{
			Dir:         filepath.Clean(dir),
			PackagePath: packagePath,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return packages, nil
}

func skipWorkspaceWalkDir(name string) bool {
	switch name {
	case "node_modules", ".git", ".hg", ".svn", ".cache", ".next", ".turbo":
		return true
	default:
		return false
	}
}

func skipGeneratedWorkspaceDir(root, dir, name string) bool {
	switch name {
	case "dist", "build", "coverage", "out":
		return hasPackageJSONAncestor(root, filepath.Dir(dir))
	default:
		return false
	}
}

func hasPackageJSONAncestor(root, dir string) bool {
	root = filepath.Clean(root)
	dir = filepath.Clean(dir)
	for {
		if samePath(dir, root) {
			return false
		}
		if _, err := os.Stat(filepath.Join(dir, "package.json")); err == nil {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

func workspacePackageMatchesPattern(root, pattern, dir, packagePath string) bool {
	relDir, err := filepath.Rel(root, dir)
	if err != nil {
		return false
	}
	relPackage, err := filepath.Rel(root, packagePath)
	if err != nil {
		return false
	}
	relDir = filepath.ToSlash(relDir)
	relPackage = filepath.ToSlash(relPackage)
	return matchGlobstarPattern(pattern, relDir) || matchGlobstarPattern(pattern, relPackage)
}

func matchGlobstarPattern(pattern, value string) bool {
	pattern = strings.Trim(pattern, "/")
	value = strings.Trim(value, "/")
	patternParts := splitPathPattern(pattern)
	valueParts := splitPathPattern(value)
	return matchGlobstarParts(patternParts, valueParts)
}

func splitPathPattern(value string) []string {
	if value == "" || value == "." {
		return nil
	}
	return strings.Split(value, "/")
}

func matchGlobstarParts(pattern, value []string) bool {
	if len(pattern) == 0 {
		return len(value) == 0
	}
	if pattern[0] == "**" {
		if matchGlobstarParts(pattern[1:], value) {
			return true
		}
		return len(value) > 0 && matchGlobstarParts(pattern, value[1:])
	}
	if len(value) == 0 {
		return false
	}
	ok, err := path.Match(pattern[0], value[0])
	if err != nil || !ok {
		return false
	}
	return matchGlobstarParts(pattern[1:], value[1:])
}

func workspacePatterns(root string) ([]string, bool, error) {
	pnpmPath := filepath.Join(root, "pnpm-workspace.yaml")
	if data, err := os.ReadFile(pnpmPath); err == nil {
		return parsePNPMWorkspacePackages(string(data)), true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}

	pkg, err := ReadPackageJSON(filepath.Join(root, "package.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return packageJSONWorkspacePatterns(pkg.Workspaces)
}

func packageJSONWorkspacePatterns(raw json.RawMessage) ([]string, bool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false, nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return list, true, nil
	}
	var object struct {
		Packages []string `json:"packages"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, true, err
	}
	return object.Packages, true, nil
}

func parsePNPMWorkspacePackages(data string) []string {
	var patterns []string
	inPackages := false
	for _, line := range strings.Split(data, "\n") {
		withoutComment := stripYAMLComment(line)
		trimmed := strings.TrimSpace(withoutComment)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			inPackages = false
			key, value, ok := strings.Cut(trimmed, ":")
			if !ok || strings.TrimSpace(key) != "packages" {
				continue
			}
			inPackages = true
			value = strings.TrimSpace(value)
			if value != "" {
				patterns = append(patterns, parseYAMLPatternValues(value)...)
				inPackages = false
			}
			continue
		}
		if !inPackages || !strings.HasPrefix(trimmed, "- ") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))
		patterns = append(patterns, parseYAMLPatternValues(value)...)
	}
	return patterns
}

func parseYAMLPatternValues(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		var patterns []string
		for _, item := range splitYAMLInlineList(strings.TrimSpace(value[1 : len(value)-1])) {
			if pattern := trimYAMLScalar(item); pattern != "" {
				patterns = append(patterns, pattern)
			}
		}
		return patterns
	}
	if pattern := trimYAMLScalar(value); pattern != "" {
		return []string{pattern}
	}
	return nil
}

func splitYAMLInlineList(value string) []string {
	if value == "" {
		return nil
	}
	var parts []string
	var current strings.Builder
	var quote rune
	for _, r := range value {
		if quote != 0 {
			current.WriteRune(r)
			if r == quote {
				quote = 0
			}
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
			current.WriteRune(r)
		case ',':
			parts = append(parts, current.String())
			current.Reset()
		default:
			current.WriteRune(r)
		}
	}
	parts = append(parts, current.String())
	return parts
}

func trimYAMLScalar(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		quote := value[0]
		if (quote == '\'' || quote == '"') && value[len(value)-1] == quote {
			value = value[1 : len(value)-1]
		}
	}
	return strings.TrimSpace(value)
}

func stripYAMLComment(line string) string {
	var quote rune
	for i, r := range line {
		if quote != 0 {
			if r == quote {
				quote = 0
			}
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
		case '#':
			return line[:i]
		}
	}
	return line
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

	for suffix := 2; suffix <= maxHostnameConflictAttempts; suffix++ {
		candidate = conflictHost(parent, base, suffix)
		if existingCWD, ok := activeHostCWD(active, candidate); !ok || samePath(existingCWD, cwd) {
			return candidate
		}
	}
	for attempt := 0; attempt <= maxHostnameConflictAttempts; attempt++ {
		candidate = fallbackConflictHost(parent, base, cwd, attempt)
		if existingCWD, ok := activeHostCWD(active, candidate); !ok || samePath(existingCWD, cwd) {
			return candidate
		}
	}
	return fallbackConflictHost(parent, base, cwd, maxHostnameConflictAttempts+1)
}

func conflictHost(parent, base string, suffix int) string {
	label := parent + "-" + base
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

func fallbackConflictHost(parent, base, cwd string, attempt int) string {
	suffixPart := "-" + shortHash(fmt.Sprintf("%s|%s|%s|%d", parent, base, cwd, attempt))
	label := NormalizeHostnameName(parent + "-" + base)
	if len(label)+len(suffixPart) > 63 {
		label = strings.TrimRight(label[:63-len(suffixPart)], "-")
	}
	if label == "" {
		label = "app"
	}
	return label + suffixPart + ".localhost"
}

func shortHash(value string) string {
	sum := sha1.Sum([]byte(value))
	return hex.EncodeToString(sum[:])[:8]
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
	absA, errA := canonicalPath(a)
	absB, errB := canonicalPath(b)
	return errA == nil && errB == nil && absA == absB
}

func canonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return resolved, nil
	}
	return abs, nil
}
