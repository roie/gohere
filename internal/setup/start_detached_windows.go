//go:build windows

package setup

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

func (realRunner) StartDetached(ctx context.Context, command string, args ...string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	script := windowsStartProcessScript(command, args)
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("start detached service: %w: %s", err, strings.TrimSpace(string(output)))
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil {
		return 0, fmt.Errorf("start detached service: invalid pid %q", strings.TrimSpace(string(output)))
	}
	return pid, nil
}

func windowsStartProcessScript(command string, args []string) string {
	var b strings.Builder
	b.WriteString("$ErrorActionPreference = 'Stop'; ")
	b.WriteString("$p = Start-Process -FilePath ")
	b.WriteString(powershellSingleQuote(command))
	if len(args) > 0 {
		b.WriteString(" -ArgumentList @(")
		for i, arg := range args {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(powershellSingleQuote(arg))
		}
		b.WriteString(")")
	}
	b.WriteString(" -WindowStyle Hidden -PassThru; $p.Id")
	return b.String()
}

func powershellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
