package companion

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBundledWindowsBinaryFindsNPMVendorCompanion(t *testing.T) {
	root := t.TempDir()
	linuxBinary := filepath.Join(root, "vendor", "linux-x64", "gohere")
	windowsBinary := filepath.Join(root, "vendor", "win32-x64", "gohere.exe")
	writeTestFile(t, linuxBinary, "linux")
	writeTestFile(t, windowsBinary, "windows")

	got, err := BundledWindowsBinary(linuxBinary, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(windowsBinary)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("binary = %q, want %q", got, want)
	}
}

func TestBundledWindowsBinaryRequiresNPMLayout(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "bin", "gohere")
	writeTestFile(t, binary, "linux")

	_, err := BundledWindowsBinary(binary, "amd64")
	if err == nil || !strings.Contains(err.Error(), "install gohere through npm") {
		t.Fatalf("error = %v", err)
	}
}

func TestBundledWindowsBinaryRejectsUnsupportedArchitecture(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "vendor", "linux-riscv64", "gohere")
	writeTestFile(t, binary, "linux")

	_, err := BundledWindowsBinary(binary, "riscv64")
	if err == nil || !strings.Contains(err.Error(), `architecture "riscv64"`) {
		t.Fatalf("error = %v", err)
	}
}

func TestStageWindowsBinaryCopiesAndRepairsContentAddressedCompanion(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "npm", "gohere.exe")
	writeTestFile(t, source, "signed-windows-binary")
	stageRoot := filepath.Join(root, "windows-temp")
	runner := &stageOutputRunner{linuxTemp: stageRoot}

	destination, err := StageWindowsBinary(t.Context(), source, runner)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(destination, filepath.Join(stageRoot, "gohere", "companion")) || !strings.HasSuffix(destination, ".exe") {
		t.Fatalf("destination = %q", destination)
	}
	assertFileContent(t, destination, "signed-windows-binary")
	if runtime.GOOS != "windows" {
		info, err := os.Stat(destination)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0100 == 0 {
			t.Fatalf("staged mode = %v", info.Mode())
		}
	}

	writeTestFile(t, destination, "tampered")
	repaired, err := StageWindowsBinary(t.Context(), source, runner)
	if err != nil {
		t.Fatal(err)
	}
	if repaired != destination {
		t.Fatalf("repaired path = %q, want %q", repaired, destination)
	}
	assertFileContent(t, repaired, "signed-windows-binary")
	if runner.cmdCalls != 2 || runner.wslpathCalls != 2 {
		t.Fatalf("calls = cmd %d, wslpath %d", runner.cmdCalls, runner.wslpathCalls)
	}
}

func TestStageWindowsBinaryReportsWindowsTempFailure(t *testing.T) {
	source := filepath.Join(t.TempDir(), "gohere.exe")
	writeTestFile(t, source, "windows")
	runner := &stageOutputRunner{err: errors.New("interop unavailable")}

	_, err := StageWindowsBinary(t.Context(), source, runner)
	if err == nil || !strings.Contains(err.Error(), "resolve the current Windows temporary directory") {
		t.Fatalf("error = %v", err)
	}
}

type stageOutputRunner struct {
	linuxTemp    string
	err          error
	cmdCalls     int
	wslpathCalls int
}

func (r *stageOutputRunner) Output(_ context.Context, command string, args ...string) ([]byte, error) {
	if r.err != nil {
		return nil, r.err
	}
	switch command {
	case "cmd.exe":
		if len(args) != 4 || args[0] != "/d" || args[1] != "/s" || args[2] != "/c" || args[3] != "echo %TEMP%" {
			return nil, errors.New("unexpected cmd arguments")
		}
		r.cmdCalls++
		return []byte(`C:\Users\Alice\AppData\Local\Temp\`), nil
	case "wslpath":
		r.wslpathCalls++
		if len(args) != 2 || args[0] != "-u" {
			return nil, errors.New("unexpected wslpath arguments")
		}
		return []byte(r.linuxTemp + "\n"), nil
	default:
		return nil, errors.New("unexpected command: " + command)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatal(err)
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("content = %q, want %q", data, want)
	}
}
