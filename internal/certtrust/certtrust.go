package certtrust

import (
	"context"
	"fmt"
	"strings"
)

type CommandRunner interface {
	Run(ctx context.Context, command string, args ...string) error
}

func UntrustCA(ctx context.Context, goos string, runner CommandRunner, fingerprint string) error {
	if runner == nil {
		return fmt.Errorf("certificate trust command runner is required")
	}
	fingerprint = strings.TrimSpace(fingerprint)
	switch goos {
	case "linux":
		if err := runner.Run(ctx, "sudo", "rm", "-f", "/usr/local/share/ca-certificates/gohere-local-ca.crt"); err != nil {
			return err
		}
		return runner.Run(ctx, "sudo", "update-ca-certificates")
	case "darwin":
		if fingerprint == "" {
			return fmt.Errorf("certificate fingerprint is required")
		}
		return runner.Run(ctx, "sudo", "security", "delete-certificate", "-Z", fingerprint, "/Library/Keychains/System.keychain")
	case "windows":
		if fingerprint == "" {
			return fmt.Errorf("certificate fingerprint is required")
		}
		return runner.Run(ctx, "certutil", "-user", "-delstore", "Root", fingerprint)
	default:
		return fmt.Errorf("gohere HTTPS uninstall is not supported on %s yet", goos)
	}
}

func TrustCA(ctx context.Context, goos string, runner CommandRunner, caPath string) error {
	if runner == nil {
		return fmt.Errorf("certificate trust command runner is required")
	}
	switch goos {
	case "linux":
		if err := runner.Run(ctx, "sudo", "mkdir", "-p", "/usr/local/share/ca-certificates"); err != nil {
			return err
		}
		if err := runner.Run(ctx, "sudo", "cp", caPath, "/usr/local/share/ca-certificates/gohere-local-ca.crt"); err != nil {
			return err
		}
		return runner.Run(ctx, "sudo", "update-ca-certificates")
	case "darwin":
		return runner.Run(ctx, "sudo", "security", "add-trusted-cert", "-d", "-r", "trustRoot", "-k", "/Library/Keychains/System.keychain", caPath)
	case "windows":
		return runner.Run(ctx, "certutil", "-user", "-addstore", "Root", caPath)
	default:
		return fmt.Errorf("gohere HTTPS setup is not supported on %s yet", goos)
	}
}
