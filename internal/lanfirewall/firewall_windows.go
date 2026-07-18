//go:build windows

package lanfirewall

import (
	"context"
	"encoding/base64"
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
	script := windowsFirewallScript(executable)
	if output, err := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-EncodedCommand", encodedPowerShell(script)).CombinedOutput(); err == nil {
		return nil
	} else if ctx.Err() != nil {
		return ctx.Err()
	} else {
		elevated := `$process = Start-Process -FilePath 'powershell.exe' -Verb RunAs -Wait -PassThru -ArgumentList @('-NoProfile','-NonInteractive','-EncodedCommand','` + encodedPowerShell(script) + `'); exit $process.ExitCode`
		elevatedOutput, elevatedErr := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", elevated).CombinedOutput()
		if elevatedErr != nil {
			detail := strings.TrimSpace(string(elevatedOutput))
			if detail == "" {
				detail = strings.TrimSpace(string(output))
			}
			return fmt.Errorf("permission to configure LAN sharing was not granted: %s", detail)
		}
		return nil
	}
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
