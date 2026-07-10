package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/roie/gohere/internal/certtrust"
	"github.com/roie/gohere/internal/router"
	"github.com/roie/gohere/internal/setup"
	"github.com/roie/gohere/internal/wsledge"
)

var (
	wslStopEdgeFunc                          = wsledge.Stop
	wslUntrustCARunner   setup.CommandRunner = appCommandRunner{}
	wslTrustedCAPathFunc                     = func() string { return "/usr/local/share/ca-certificates/gohere-local-ca.crt" }
)

func uninstallWSLIntegration(ctx context.Context, stdout io.Writer) error {
	if stdout == nil {
		stdout = io.Discard
	}
	stateDir := router.DefaultStateDir()
	integrationDir := filepath.Join(stateDir, wslIntegrationDirname)
	metadata, metadataErr := loadWSLIntegrationMetadata(stateDir)

	removedWindows := false
	attemptedWindowsRemoval := false
	var windowsRemovalErr error
	resolved, companionErr := openWindowsCompanion(ctx, "control.routes", "control.uninstall")
	if companionErr == nil && confirmPrompt(stdout, "Remove the shared Windows gohere router too? [y/N] ") {
		attemptedWindowsRemoval = true
		routes, err := resolved.Control.Routes(ctx)
		if err != nil {
			return err
		}
		if len(routes) > 0 {
			return fmt.Errorf("the shared Windows authority still has %d route%s; stop or prune them before removing it", len(routes), pluralS(len(routes)))
		}
		removeState := confirmPrompt(stdout, "Remove Windows routes, token, and logs too? [y/N] ")
		output, err := resolved.Control.Uninstall(ctx, removeState)
		if err != nil {
			windowsRemovalErr = windowsCompanionUnavailableError(err)
		} else {
			fmt.Fprint(stdout, output)
			removedWindows = true
		}
	}

	if err := wslStopEdgeFunc(integrationDir); err != nil {
		return err
	}
	if metadataErr == nil && metadata.CAFingerprint != "" {
		trustedPath := wslTrustedCAPathFunc()
		if certificate, err := os.ReadFile(trustedPath); err == nil {
			fingerprint, fingerprintErr := certificateFingerprint(string(certificate))
			if fingerprintErr != nil {
				return fingerprintErr
			}
			if !strings.EqualFold(fingerprint, metadata.CAFingerprint) {
				return errors.New("the installed WSL gohere CA does not match this integration; refusing to remove it")
			}
			if err := certtrust.UntrustCA(ctx, "linux", wslUntrustCARunner, metadata.CAFingerprint); err != nil {
				return err
			}
		}
	}
	if err := os.RemoveAll(integrationDir); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "gohere WSL edge and certificate trust removed.")
	if windowsRemovalErr != nil {
		fmt.Fprintln(stdout, "Windows authority removal failed; WSL cleanup completed.")
		return windowsRemovalErr
	}
	if !removedWindows {
		if attemptedWindowsRemoval {
			fmt.Fprintln(stdout, "Windows gohere authority may require cleanup.")
		} else {
			fmt.Fprintln(stdout, "Windows gohere authority kept.")
		}
	}
	if companionErr != nil {
		fmt.Fprintf(stdout, "Windows authority could not be inspected: %v\n", companionErr)
	}
	return nil
}

func confirmPrompt(output io.Writer, prompt string) bool {
	fmt.Fprint(output, prompt)
	answer, _ := bufio.NewReader(promptInput).ReadString('\n')
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}
