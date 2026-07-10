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
)

type OutputRunner interface {
	Output(context.Context, string, ...string) ([]byte, error)
}

type ExecOutputRunner struct{}

func (ExecOutputRunner) Output(ctx context.Context, command string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, command, args...).Output()
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
	if runner == nil {
		runner = ExecOutputRunner{}
	}
	sourceHash, err := fileSHA256(source)
	if err != nil {
		return "", err
	}
	windowsTemp, err := runner.Output(
		ctx,
		"cmd.exe",
		"/d",
		"/s",
		"/c",
		"echo %TEMP%",
	)
	if err != nil {
		return "", fmt.Errorf("could not resolve the current Windows temporary directory: %w", err)
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
