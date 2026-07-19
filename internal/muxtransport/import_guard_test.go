package muxtransport

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const muxImportPath = "github.com/libp2p/go-yamux/v5"

func TestNoDirectMuxImports(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	violations, err := directMuxImports(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("yamux must only be imported by internal/muxtransport; direct imports: %v", violations)
	}
}

func TestDirectMuxImportGuardDetectsViolation(t *testing.T) {
	root := t.TempDir()
	allowedDir := filepath.Join(root, "internal", "muxtransport")
	forbiddenDir := filepath.Join(root, "internal", "tunnel")
	worktreeDir := filepath.Join(root, ".worktrees", "feature", "internal", "tunnel")
	if err := os.MkdirAll(allowedDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(forbiddenDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(worktreeDir, 0700); err != nil {
		t.Fatal(err)
	}
	allowedFile := filepath.Join(allowedDir, "allowed.go")
	forbiddenFile := filepath.Join(forbiddenDir, "forbidden.go")
	worktreeFile := filepath.Join(worktreeDir, "ignored.go")
	if err := os.WriteFile(allowedFile, []byte("package muxtransport\nimport _ \""+muxImportPath+"\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(forbiddenFile, []byte("package tunnel\nimport _ \""+muxImportPath+"\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(worktreeFile, []byte("package tunnel\nimport _ \""+muxImportPath+"\"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	violations, err := directMuxImports(root)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("internal", "tunnel", "forbidden.go")
	if !slices.Equal(violations, []string{want}) {
		t.Fatalf("violations = %v, want %v", violations, []string{want})
	}
}

func directMuxImports(root string) ([]string, error) {
	allowedDir := filepath.Clean(filepath.Join(root, "internal", "muxtransport"))
	var violations []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".worktrees", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imported := range parsed.Imports {
			importPath, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				return err
			}
			if importPath != muxImportPath || pathWithin(filepath.Dir(path), allowedDir) {
				continue
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			violations = append(violations, relative)
		}
		return nil
	})
	slices.Sort(violations)
	return violations, err
}

func pathWithin(path, parent string) bool {
	relative, err := filepath.Rel(parent, path)
	return err == nil && relative != ".." && !filepath.IsAbs(relative) &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
