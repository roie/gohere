package opener

import (
	"context"
	"fmt"
	"os/exec"
)

func CommandFor(goos string, wsl bool, url string) []string {
	if wsl {
		return []string{"cmd.exe", "/c", "start", "", url}
	}
	switch goos {
	case "linux":
		return []string{"xdg-open", url}
	case "darwin":
		return []string{"open", url}
	case "windows":
		return []string{"rundll32", "url.dll,FileProtocolHandler", url}
	default:
		return nil
	}
}

func Open(ctx context.Context, goos string, wsl bool, url string) error {
	command := CommandFor(goos, wsl, url)
	if len(command) == 0 {
		return fmt.Errorf("browser opening is not supported on %s", goos)
	}
	return exec.CommandContext(ctx, command[0], command[1:]...).Start()
}
