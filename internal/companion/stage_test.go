package companion

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	runner := &stageOutputRunner{cmdResults: []stageOutputResult{{err: errors.New("interop unavailable")}}}

	_, err := StageWindowsBinary(t.Context(), source, runner)
	if err == nil || !strings.Contains(err.Error(), "resolve the current Windows temporary directory") {
		t.Fatalf("error = %v", err)
	}
}

type stageOutputRunner struct {
	linuxTemp    string
	cmdResults   []stageOutputResult
	wslpathErr   error
	cmdCalls     int
	wslpathCalls int
	cancelOnCmd  func(int)
}

type stageOutputResult struct {
	output []byte
	err    error
}

func (r *stageOutputRunner) Output(ctx context.Context, command string, args ...string) ([]byte, error) {
	switch command {
	case "cmd.exe":
		if len(args) != 4 || args[0] != "/d" || args[1] != "/s" || args[2] != "/c" || args[3] != "echo %TEMP%" {
			return nil, errors.New("unexpected cmd arguments")
		}
		r.cmdCalls++
		if r.cancelOnCmd != nil {
			r.cancelOnCmd(r.cmdCalls)
		}
		if len(r.cmdResults) > 0 {
			result := r.cmdResults[0]
			r.cmdResults = r.cmdResults[1:]
			return result.output, result.err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return []byte(`C:\Users\Alice\AppData\Local\Temp\`), nil
	case "wslpath":
		r.wslpathCalls++
		if len(args) != 2 || args[0] != "-u" {
			return nil, errors.New("unexpected wslpath arguments")
		}
		if r.wslpathErr != nil {
			return nil, r.wslpathErr
		}
		return []byte(r.linuxTemp + "\n"), nil
	default:
		return nil, errors.New("unexpected command: " + command)
	}
}

func stageExitError(t *testing.T, stderr string) *exec.ExitError {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestStageExitHelper$")
	cmd.Env = append(os.Environ(), "GOHERE_STAGE_EXIT_HELPER=1")
	_, err := cmd.Output()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("helper error = %T %v, want *exec.ExitError", err, err)
	}
	exitErr.Stderr = []byte(stderr)
	return exitErr
}

func TestStageExitHelper(t *testing.T) {
	if os.Getenv("GOHERE_STAGE_EXIT_HELPER") == "1" {
		os.Exit(7)
	}
}

func TestExecOutputRunnerCapturesBoundedStderrPrefixAndSuffix(t *testing.T) {
	t.Setenv("GOHERE_STAGE_OUTPUT_HELPER", "1")
	_, err := (ExecOutputRunner{}).Output(t.Context(), os.Args[0], "-test.run=^TestStageOutputHelper$")
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("error = %T %v, want *exec.ExitError", err, err)
	}
	if len(exitErr.Stderr) > 66*1024 {
		t.Fatalf("captured stderr length = %d, want bounded capture", len(exitErr.Stderr))
	}
	if !strings.Contains(string(exitErr.Stderr), "diagnostic-prefix") ||
		!strings.Contains(string(exitErr.Stderr), "diagnostic-suffix") ||
		!strings.Contains(string(exitErr.Stderr), "omitting") {
		t.Fatalf("captured stderr did not retain both ends: %q", exitErr.Stderr)
	}
}

func TestStageOutputHelper(t *testing.T) {
	if os.Getenv("GOHERE_STAGE_OUTPUT_HELPER") != "1" {
		return
	}
	fmt.Fprint(os.Stderr, "diagnostic-prefix ")
	fmt.Fprint(os.Stderr, strings.Repeat("x", 96*1024))
	fmt.Fprint(os.Stderr, " diagnostic-suffix")
	os.Exit(9)
}

func TestCompactCommandDetailRetainsBothEndsWithinPresentationLimit(t *testing.T) {
	detail := "diagnostic-prefix \n\t" + strings.Repeat("x", 4*1024) + " diagnostic-suffix"
	got := compactCommandDetail(detail)
	if len(got) > 2*1024 {
		t.Fatalf("detail length = %d, want at most 2048", len(got))
	}
	if !strings.HasPrefix(got, "diagnostic-prefix ") ||
		!strings.HasSuffix(got, " diagnostic-suffix") ||
		!strings.Contains(got, "omitted") ||
		strings.ContainsAny(got, "\n\t") {
		t.Fatalf("detail = %q", got)
	}
}

func TestStageWindowsBinaryRetryAndExitBehavior(t *testing.T) {
	noDelay := func(context.Context) error { return nil }

	tests := []struct {
		name string
		run  func(*testing.T, string)
	}{
		{
			name: "transient exit retries once",
			run: func(t *testing.T, source string) {
				runner := &stageOutputRunner{
					linuxTemp: t.TempDir(),
					cmdResults: []stageOutputResult{
						{err: stageExitError(t, "")},
						{output: []byte(`C:\Users\Alice\AppData\Local\Temp\`)},
					},
				}
				if _, err := stageWindowsBinary(t.Context(), source, runner, noDelay); err != nil {
					t.Fatal(err)
				}
				if runner.cmdCalls != 2 || runner.wslpathCalls != 1 {
					t.Fatalf("calls = cmd %d, wslpath %d", runner.cmdCalls, runner.wslpathCalls)
				}
			},
		},
		{
			name: "wording changed retries without text recognition",
			run: func(t *testing.T, source string) {
				runner := &stageOutputRunner{
					linuxTemp: t.TempDir(),
					cmdResults: []stageOutputResult{
						{err: stageExitError(t, "future WSL wording")},
						{output: []byte(`C:\Users\Alice\AppData\Local\Temp\`)},
					},
				}
				if _, err := stageWindowsBinary(t.Context(), source, runner, noDelay); err != nil {
					t.Fatal(err)
				}
				if runner.cmdCalls != 2 || runner.wslpathCalls != 1 {
					t.Fatalf("calls = cmd %d, wslpath %d", runner.cmdCalls, runner.wslpathCalls)
				}
			},
		},
		{
			name: "persistent recognized exit reports guidance and wraps second error",
			run: func(t *testing.T, source string) {
				firstErr := stageExitError(t, "UtilBindVsockAnyPort first")
				secondErr := stageExitError(t, "")
				runner := &stageOutputRunner{cmdResults: []stageOutputResult{{err: firstErr}, {err: secondErr}}}
				_, err := stageWindowsBinary(t.Context(), source, runner, noDelay)
				if err == nil {
					t.Fatal("expected error")
				}
				var gotExit *exec.ExitError
				if !errors.As(err, &gotExit) || gotExit != secondErr {
					t.Fatalf("wrapped exit error = %p, want second %p", gotExit, secondErr)
				}
				for _, want := range []string{
					"UtilBindVsockAnyPort first",
					"cmd.exe /d /c ver",
					"Windows PowerShell or Command Prompt",
					"wsl --update",
					"wsl --shutdown",
					"did not start or fall back to a WSL router",
				} {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("error missing %q: %v", want, err)
					}
				}
			},
		},
		{
			name: "second recognized exit wins",
			run: func(t *testing.T, source string) {
				runner := &stageOutputRunner{cmdResults: []stageOutputResult{
					{err: stageExitError(t, "UtilBindVsockAnyPort first")},
					{err: stageExitError(t, "UtilBindVsockAnyPort second")},
				}}
				_, err := stageWindowsBinary(t.Context(), source, runner, noDelay)
				if err == nil || !strings.Contains(err.Error(), "UtilBindVsockAnyPort second") || strings.Contains(err.Error(), "UtilBindVsockAnyPort first") {
					t.Fatalf("error = %v", err)
				}
			},
		},
		{
			name: "generic persistent exit reports second diagnostic without WSL update guidance",
			run: func(t *testing.T, source string) {
				runner := &stageOutputRunner{cmdResults: []stageOutputResult{
					{err: stageExitError(t, "first generic")},
					{err: stageExitError(t, "second generic")},
				}}
				_, err := stageWindowsBinary(t.Context(), source, runner, noDelay)
				if err == nil || !strings.Contains(err.Error(), "second generic") || strings.Contains(err.Error(), "wsl --update") {
					t.Fatalf("error = %v", err)
				}
			},
		},
		{
			name: "unavailable executable does not retry",
			run: func(t *testing.T, source string) {
				runner := &stageOutputRunner{cmdResults: []stageOutputResult{{err: &exec.Error{Name: "cmd.exe", Err: exec.ErrNotFound}}}}
				_, err := stageWindowsBinary(t.Context(), source, runner, noDelay)
				if !errors.Is(err, exec.ErrNotFound) {
					t.Fatalf("error = %v, want ErrNotFound", err)
				}
				if runner.cmdCalls != 1 {
					t.Fatalf("cmd calls = %d, want 1", runner.cmdCalls)
				}
			},
		},
		{
			name: "cancelled before exit classification stops immediately",
			run: func(t *testing.T, source string) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				runner := &stageOutputRunner{
					cmdResults:  []stageOutputResult{{err: stageExitError(t, "")}},
					cancelOnCmd: func(int) { cancel() },
				}
				_, err := stageWindowsBinary(ctx, source, runner, noDelay)
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("error = %v, want context.Canceled", err)
				}
				if runner.cmdCalls != 1 {
					t.Fatalf("cmd calls = %d, want 1", runner.cmdCalls)
				}
			},
		},
		{
			name: "cancelled during retry delay stops immediately",
			run: func(t *testing.T, source string) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				runner := &stageOutputRunner{cmdResults: []stageOutputResult{{err: stageExitError(t, "")}}}
				wait := func(ctx context.Context) error {
					cancel()
					return ctx.Err()
				}
				_, err := stageWindowsBinary(ctx, source, runner, wait)
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("error = %v, want context.Canceled", err)
				}
				if runner.cmdCalls != 1 {
					t.Fatalf("cmd calls = %d, want 1", runner.cmdCalls)
				}
			},
		},
		{
			name: "empty successful output is rejected",
			run: func(t *testing.T, source string) {
				runner := &stageOutputRunner{cmdResults: []stageOutputResult{{}}}
				_, err := stageWindowsBinary(t.Context(), source, runner, noDelay)
				if err == nil || !strings.Contains(err.Error(), "Windows returned an empty temporary directory") {
					t.Fatalf("error = %v", err)
				}
				if runner.cmdCalls != 1 {
					t.Fatalf("cmd calls = %d, want 1", runner.cmdCalls)
				}
			},
		},
		{
			name: "path conversion failure preserves mapping error",
			run: func(t *testing.T, source string) {
				runner := &stageOutputRunner{linuxTemp: t.TempDir(), wslpathErr: errors.New("cannot convert path")}
				_, err := stageWindowsBinary(t.Context(), source, runner, noDelay)
				if err == nil || !strings.Contains(err.Error(), "could not map the Windows temporary directory into WSL") {
					t.Fatalf("error = %v", err)
				}
				if runner.cmdCalls != 1 || runner.wslpathCalls != 1 {
					t.Fatalf("calls = cmd %d, wslpath %d", runner.cmdCalls, runner.wslpathCalls)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := filepath.Join(t.TempDir(), "gohere.exe")
			writeTestFile(t, source, "windows")
			test.run(t, source)
		})
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
