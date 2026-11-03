package certtrust

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"slices"
	"strings"
)

const linuxCAFilename = "gohere-local-ca.crt"

type linuxTrustStore struct {
	dir     string
	command []string
	// createDir allows the Debian layout when ca-certificates has not created its directory yet.
	createDir bool
}

// linuxTrustStores is ordered so that a distribution-specific anchor directory
// wins over the Debian fallback, which only needs update-ca-certificates.
var linuxTrustStores = []linuxTrustStore{
	{dir: "/usr/local/share/ca-certificates", command: []string{"update-ca-certificates"}},
	{dir: "/etc/pki/ca-trust/source/anchors", command: []string{"update-ca-trust", "extract"}},
	{dir: "/etc/ca-certificates/trust-source/anchors", command: []string{"update-ca-trust", "extract"}},
	{dir: "/etc/pki/trust/anchors", command: []string{"update-ca-certificates"}},
	{dir: "/usr/local/share/ca-certificates", command: []string{"update-ca-certificates"}, createDir: true},
}

var errNoLinuxTrustStore = errors.New("no supported system trust store found; gohere needs either update-ca-certificates (Debian, Ubuntu, Alpine, openSUSE) or update-ca-trust (Fedora, RHEL, CentOS, Arch). Install your distribution's ca-certificates package, then run gohere again")

// LinuxHost inspects the machine for trust stores; tests replace it to simulate a distribution.
var LinuxHost = struct {
	DirExists     func(string) bool
	FileExists    func(string) bool
	ReadFile      func(string) ([]byte, error)
	CommandExists func(string) bool
}{
	DirExists: func(path string) bool {
		info, err := os.Stat(path)
		return err == nil && info.IsDir()
	},
	FileExists: func(path string) bool {
		info, err := os.Stat(path)
		return err == nil && info.Mode().IsRegular()
	},
	ReadFile: os.ReadFile,
	CommandExists: func(name string) bool {
		if _, err := exec.LookPath(name); err == nil {
			return true
		}
		// sudo's secure_path includes sbin directories that a normal user's PATH may not.
		for _, dir := range []string{"/usr/sbin", "/usr/bin", "/sbin", "/bin"} {
			if info, err := os.Stat(path.Join(dir, name)); err == nil && info.Mode().IsRegular() {
				return true
			}
		}
		return false
	},
}

func detectLinuxTrustStore() (linuxTrustStore, error) {
	for _, store := range linuxTrustStores {
		if LinuxHost.CommandExists(store.command[0]) && (store.createDir || LinuxHost.DirExists(store.dir)) {
			return store, nil
		}
	}
	return linuxTrustStore{}, errNoLinuxTrustStore
}

// InstalledLinuxCAPath returns the gohere CA file in a known Linux trust
// store, or "" when none is installed.
func InstalledLinuxCAPath() string {
	for _, path := range linuxCAPaths() {
		if LinuxHost.FileExists(path) {
			return path
		}
	}
	return ""
}

func linuxCAPaths() []string {
	var paths []string
	for _, store := range linuxTrustStores {
		candidate := path.Join(store.dir, linuxCAFilename)
		if !slices.Contains(paths, candidate) {
			paths = append(paths, candidate)
		}
	}
	return paths
}

// verifyLinuxCA accepts a SHA-256 or SHA-1 hex fingerprint: native Linux
// passes SHA-256, while WSL passes the SHA-1 thumbprint it also gives certutil.
func verifyLinuxCA(path, fingerprint string) error {
	data, err := LinuxHost.ReadFile(path)
	if err != nil {
		return fmt.Errorf("could not verify %s, so it was left in place: %w", path, err)
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return fmt.Errorf("%s is not a PEM certificate, so it was left in place; remove it manually if gohere installed it", path)
	}
	sha256Sum := sha256.Sum256(block.Bytes)
	sha1Sum := sha1.Sum(block.Bytes)
	if !strings.EqualFold(fingerprint, hex.EncodeToString(sha256Sum[:])) && !strings.EqualFold(fingerprint, hex.EncodeToString(sha1Sum[:])) {
		return fmt.Errorf("%s is not the certificate authority this gohere installation trusted, so it was left in place; remove it manually if it is no longer needed", path)
	}
	return nil
}

func refreshLinuxTrust(ctx context.Context, runner CommandRunner, store linuxTrustStore) error {
	return runner.Run(ctx, "sudo", store.command...)
}

func trustLinuxCA(ctx context.Context, runner CommandRunner, caPath string) error {
	store, err := detectLinuxTrustStore()
	if err != nil {
		return err
	}
	target := path.Join(store.dir, linuxCAFilename)
	if store.createDir {
		if err := runner.Run(ctx, "sudo", "mkdir", "-p", store.dir); err != nil {
			return err
		}
	}
	if err := runner.Run(ctx, "sudo", "cp", caPath, target); err != nil {
		return errors.Join(err, rollbackLinuxCA(ctx, runner, store, target))
	}
	if err := refreshLinuxTrust(ctx, runner, store); err != nil {
		return errors.Join(err, rollbackLinuxCA(ctx, runner, store, target))
	}
	return nil
}

// rollbackLinuxCA removes a copied CA so a failed install does not leave an
// anchor that the next system trust refresh would silently trust.
func rollbackLinuxCA(ctx context.Context, runner CommandRunner, store linuxTrustStore, target string) error {
	if err := runner.Run(ctx, "sudo", "rm", "-f", target); err != nil {
		return fmt.Errorf("could not roll back %s; remove it and run sudo %s: %w", target, strings.Join(store.command, " "), err)
	}
	if err := refreshLinuxTrust(ctx, runner, store); err != nil {
		return fmt.Errorf("removed %s but could not refresh the system trust store; run sudo %s: %w", target, strings.Join(store.command, " "), err)
	}
	return nil
}

// untrustLinuxCA removes only CA files that match fingerprint. Every installed
// file is verified before any is removed, so a mismatch changes nothing.
func untrustLinuxCA(ctx context.Context, runner CommandRunner, fingerprint string) error {
	if fingerprint == "" {
		return fmt.Errorf("certificate fingerprint is required")
	}
	var installed []string
	for _, path := range linuxCAPaths() {
		if !LinuxHost.FileExists(path) {
			continue
		}
		if err := verifyLinuxCA(path, fingerprint); err != nil {
			return err
		}
		installed = append(installed, path)
	}
	removed := false
	for _, path := range installed {
		if err := runner.Run(ctx, "sudo", "rm", "-f", path); err != nil {
			return err
		}
		removed = true
	}
	store, err := detectLinuxTrustStore()
	if err != nil {
		if removed {
			return fmt.Errorf("removed the gohere CA file, but %w", err)
		}
		return nil
	}
	return refreshLinuxTrust(ctx, runner, store)
}

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
		return untrustLinuxCA(ctx, runner, fingerprint)
	case "darwin":
		if fingerprint == "" {
			return fmt.Errorf("certificate fingerprint is required")
		}
		return runner.Run(ctx, "sudo", "security", "delete-certificate", "-Z", fingerprint, "/Library/Keychains/System.keychain")
	case "windows":
		if fingerprint == "" {
			return fmt.Errorf("certificate fingerprint is required")
		}
		return runner.Run(ctx, "certutil", "-f", "-user", "-delstore", "Root", fingerprint)
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
		return trustLinuxCA(ctx, runner, caPath)
	case "darwin":
		return runner.Run(ctx, "sudo", "security", "add-trusted-cert", "-d", "-r", "trustRoot", "-k", "/Library/Keychains/System.keychain", caPath)
	case "windows":
		return runner.Run(ctx, "certutil", "-f", "-user", "-addstore", "Root", caPath)
	default:
		return fmt.Errorf("gohere HTTPS setup is not supported on %s yet", goos)
	}
}
