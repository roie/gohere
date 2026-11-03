//go:build windows

package lanfirewall

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"unicode/utf16"
)

func Ensure(ctx context.Context) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if err := runElevatedOnFailure(ctx, windowsFirewallScript(executable)); err != nil {
		return fmt.Errorf("permission to configure LAN sharing was not granted: %w", err)
	}
	return nil
}

// Remove deletes the LAN sharing firewall rules that allow executable. Rules
// owned by another program are kept. Elevation is requested only when a
// matching rule exists and cannot be removed without it.
func Remove(ctx context.Context, executable string) error {
	return runElevatedOnFailure(ctx, windowsFirewallRemovalScript(executable))
}

func runElevatedOnFailure(ctx context.Context, script string) error {
	output, err := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-EncodedCommand", encodedPowerShell(script)).CombinedOutput()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	elevated := `$process = Start-Process -FilePath 'powershell.exe' -Verb RunAs -Wait -PassThru -ArgumentList @('-NoProfile','-NonInteractive','-EncodedCommand','` + encodedPowerShell(script) + `'); exit $process.ExitCode`
	elevatedOutput, elevatedErr := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", elevated).CombinedOutput()
	if elevatedErr == nil {
		return nil
	}
	detail := strings.TrimSpace(string(elevatedOutput))
	if detail == "" {
		detail = strings.TrimSpace(string(output))
	}
	return errors.New(detail)
}

func encodedPowerShell(script string) string {
	encoded := utf16.Encode([]rune(script))
	bytes := make([]byte, len(encoded)*2)
	for index, value := range encoded {
		bytes[index*2] = byte(value)
		bytes[index*2+1] = byte(value >> 8)
	}
	return base64.StdEncoding.EncodeToString(bytes)
}
