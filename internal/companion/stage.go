package companion

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type OutputRunner interface {
	Output(context.Context, string, ...string) ([]byte, error)
}

type ExecOutputRunner struct{}

func (ExecOutputRunner) Output(ctx context.Context, command string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, command, args...).Output()
}

const commandDetailLimit = 2 * 1024

func compactCommandDetail(detail string) string {
	detail = strings.Join(strings.Fields(detail), " ")
	if len(detail) <= commandDetailLimit {
		return detail
	}
	const marker = " ... diagnostic omitted ... "
	available := commandDetailLimit - len(marker)
	prefix := available / 2
	suffix := available - prefix
	return detail[:prefix] + marker + detail[len(detail)-suffix:]
}

func BundledWindowsBinary(currentExecutable, goarch string) (string, error) {
	if strings.TrimSpace(currentExecutable) == "" {
		return "", errors.New("current executable path is empty")
	}
	executable, err := filepath.EvalSymlinks(currentExecutable)
	if err != nil {
		return "", err
	}
	arch, err := npmArchitecture(goarch)
	if err != nil {
		return "", err
	}
	platformDir := filepath.Dir(executable)
	vendorDir := filepath.Dir(platformDir)
	if filepath.Base(vendorDir) != "vendor" {
		return "", errors.New("this WSL installation does not include the Windows companion; install gohere through npm")
	}
	binary := filepath.Join(vendorDir, "win32-"+arch, "gohere.exe")
	info, err := os.Stat(binary)
	if err != nil {
		return "", fmt.Errorf("Windows companion is missing from the npm package: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("Windows companion is not a regular file: %s", binary)
	}
	return binary, nil
}

func StageWindowsBinary(ctx context.Context, source string, runner OutputRunner) (string, error) {
	return stageWindowsBinary(ctx, source, runner, waitWindowsTempRetry)
}

const windowsTempRetryDelay = 100 * time.Millisecond

func waitWindowsTempRetry(ctx context.Context) error {
	timer := time.NewTimer(windowsTempRetryDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func stageWindowsBinary(
	ctx context.Context,
	source string,
	runner OutputRunner,
	waitRetry func(context.Context) error,
) (string, error) {
	if runner == nil {
		runner = ExecOutputRunner{}
	}
	if waitRetry == nil {
		waitRetry = waitWindowsTempRetry
	}
	sourceHash, err := fileSHA256(source)
	if err != nil {
		return "", err
	}
	windowsTemp, err := windowsTempOutput(ctx, runner, waitRetry)
	if err != nil {
		return "", err
	}
	windowsTempPath := strings.TrimSpace(string(windowsTemp))
	if windowsTempPath == "" {
		return "", errors.New("Windows returned an empty temporary directory")
	}
	linuxTemp, err := runner.Output(ctx, "wslpath", "-u", windowsTempPath)
	if err != nil {
		return "", fmt.Errorf("could not map the Windows temporary directory into WSL: %w", err)
	}
	linuxTempPath := strings.TrimSpace(string(linuxTemp))
	if !filepath.IsAbs(linuxTempPath) {
		return "", fmt.Errorf("Windows temporary directory did not map to an absolute WSL path: %q", linuxTempPath)
	}

	dir := filepath.Join(linuxTempPath, "gohere", "companion")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	destination := filepath.Join(dir, "gohere-"+hex.EncodeToString(sourceHash[:8])+".exe")
	if destinationHash, err := fileSHA256(destination); err == nil && destinationHash == sourceHash {
		return destination, nil
	}
	if err := copyExecutable(source, destination); err != nil {
		return "", err
	}
	destinationHash, err := fileSHA256(destination)
	if err != nil {
		return "", err
	}
	if destinationHash != sourceHash {
		return "", errors.New("staged Windows companion hash does not match the npm package")
	}
	return destination, nil
}

func windowsTempOutput(
	ctx context.Context,
	runner OutputRunner,
	waitRetry func(context.Context) error,
) ([]byte, error) {
	firstOutput, firstErr := runner.Output(ctx, "cmd.exe", "/d", "/s", "/c", "echo %TEMP%")
	if firstErr == nil {
		return firstOutput, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var firstExit *exec.ExitError
	if !errors.As(firstErr, &firstExit) {
		return nil, fmt.Errorf("could not resolve the current Windows temporary directory: Windows interoperability command unavailable: %w", firstErr)
	}
	if err := waitRetry(ctx); err != nil {
		return nil, err
	}
	secondOutput, secondErr := runner.Output(ctx, "cmd.exe", "/d", "/s", "/c", "echo %TEMP%")
	if secondErr == nil {
		return secondOutput, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, windowsTempExitFailure(firstErr, secondErr)
}

const windowsInteropDiagnostic = "UtilBindVsockAnyPort"

func exitDiagnostic(err error) string {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return ""
	}
	return strings.TrimSpace(string(exitErr.Stderr))
}

func preferredWindowsTempDiagnostic(firstErr, secondErr error) (string, bool) {
	first := exitDiagnostic(firstErr)
	second := exitDiagnostic(secondErr)
	switch {
	case strings.Contains(second, windowsInteropDiagnostic):
		return second, true
	case strings.Contains(first, windowsInteropDiagnostic):
		return first, true
	case second != "":
		return second, false
	default:
		return first, false
	}
}

func windowsTempExitFailure(firstErr, secondErr error) error {
	detail, interoperability := preferredWindowsTempDiagnostic(firstErr, secondErr)
	message := "could not resolve the current Windows temporary directory: Windows interoperability command started but exited unsuccessfully"
	if detail != "" {
		message += "\n\nDetails: " + compactCommandDetail(detail)
	}
	if interoperability {
		message += "\n\nWindows interoperability failed before gohere could start the bundled Windows companion."
		message += "\n\nRun from this WSL shell:\n  cmd.exe /d /c ver"
		message += "\n\nIf that succeeds only outside a sandbox or container, run first-time gohere setup from that unrestricted shell."
		message += "\n\nIf it also fails from a normal WSL shell, open Windows PowerShell or Command Prompt and run:\n  wsl --update\n  wsl --shutdown\n\nThen reopen WSL and retry."
	}
	message += "\n\nThis attempt did not start or fall back to a WSL router."
	return fmt.Errorf("%s: %w", message, secondErr)
}

func npmArchitecture(goarch string) (string, error) {
	switch goarch {
	case "amd64":
		return "x64", nil
	case "arm64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("WSL architecture %q is not supported by the npm companion", goarch)
	}
}

func fileSHA256(path string) ([sha256.Size]byte, error) {
	var result [sha256.Size]byte
	file, err := os.Open(path)
	if err != nil {
		return result, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return result, err
	}
	copy(result[:], hash.Sum(nil))
	return result, nil
}

func copyExecutable(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	temp, err := os.CreateTemp(filepath.Dir(destination), filepath.Base(destination)+".*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tempPath)
		}
	}()
	if _, err := io.Copy(temp, input); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Chmod(0755); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, destination); err != nil {
		return err
	}
	cleanup = false
	return nil
}
