//go:build windows

package setup

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

func ensureWindowsUserPath(ctx context.Context, binDir string) error {
	command := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", windowsUserPathScript(binDir))
	output, err := command.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail == "" {
			return fmt.Errorf("update Windows user PATH: %w", err)
		}
		return fmt.Errorf("update Windows user PATH: %w: %s", err, detail)
	}
	return nil
}

func RemoveWindowsUserPath(ctx context.Context, binDir string) error {
	command := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", windowsUserPathRemovalScript(binDir))
	output, err := command.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail == "" {
			return fmt.Errorf("remove Windows user PATH entry: %w", err)
		}
		return fmt.Errorf("remove Windows user PATH entry: %w: %s", err, detail)
	}
	return nil
}

func windowsUserPathScript(binDir string) string {
	quoted := "'" + strings.ReplaceAll(binDir, "'", "''") + "'"
	return "$ErrorActionPreference='Stop'; $dir=" + quoted + "; " +
		"$current=[Environment]::GetEnvironmentVariable('Path','User'); " +
		"$parts=@($current -split ';' | ForEach-Object { $_.Trim() } | Where-Object { $_ }); " +
		"if (-not ($parts | Where-Object { $_.TrimEnd('\\') -ieq $dir.TrimEnd('\\') })) { " +
		"$next=if ([string]::IsNullOrWhiteSpace($current)) { $dir } else { $current.TrimEnd(';')+';'+$dir }; " +
		"[Environment]::SetEnvironmentVariable('Path',$next,'User') }"
}

func windowsUserPathRemovalScript(binDir string) string {
	quoted := "'" + strings.ReplaceAll(binDir, "'", "''") + "'"
	return "$ErrorActionPreference='Stop'; $dir=" + quoted + "; " +
		"$current=[Environment]::GetEnvironmentVariable('Path','User'); " +
		"$parts=@($current -split ';' | ForEach-Object { $_.Trim() } | Where-Object { $_ -and $_.TrimEnd('\\') -ine $dir.TrimEnd('\\') }); " +
		"[Environment]::SetEnvironmentVariable('Path',($parts -join ';'),'User')"
}
