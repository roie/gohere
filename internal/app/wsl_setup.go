package app

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/roie/gohere/internal/certtrust"
	"github.com/roie/gohere/internal/companion"
	"github.com/roie/gohere/internal/router"
	"github.com/roie/gohere/internal/setup"
)

const (
	wslIntegrationDirname = "wsl"
	wslEdgeBinaryName     = "gohere-edge"
)

type wslIntegrationMetadata struct {
	ProtocolVersion  int    `json:"protocolVersion"`
	CompanionVersion string `json:"companionVersion"`
	EdgeBinary       string `json:"edgeBinary"`
	EdgeSHA256       string `json:"edgeSha256"`
	WindowsUser      string `json:"windowsUser"`
	WindowsStateDir  string `json:"windowsStateDir"`
	RouterInstanceID string `json:"routerInstanceId"`
	CAFingerprint    string `json:"caFingerprint"`
	Distro           string `json:"distro"`
	LinuxUser        string `json:"linuxUser"`
	OwnerInstance    string `json:"ownerInstance"`
}

type wslRunIdentity struct {
	OwnerInstance string
	Distro        string
	LinuxUser     string
	RunnerID      string
}

type wslIntegrationSetupConfig struct {
	StateDir          string
	CurrentExecutable string
	Distro            string
	LinuxUser         string
	Runner            setup.CommandRunner
	Input             io.Reader
	Output            io.Writer
}

var (
	wslSetupExecutableFunc                       = os.Executable
	wslSetupRunner           setup.CommandRunner = quietWSLSetupRunner{}
	ensureWSLRunIdentityFunc                     = ensureWSLRunIdentity
	currentWSLMetadataFunc                       = func() (wslIntegrationMetadata, error) {
		return loadWSLIntegrationMetadata(router.DefaultStateDir())
	}
)

func setupCurrentEnvironment(ctx context.Context) error {
	if detectWSLFunc() {
		return setupWSLAuthority(ctx)
	}
	return setupForGOOS(ctx, runtimeGOOS())
}

var runtimeGOOS = func() string { return runtime.GOOS }

func setupWSLAuthority(ctx context.Context) error {
	resolved, err := openWindowsCompanion(ctx, "control.bootstrap", "control.ca-certificate")
	if err != nil {
		return err
	}
	if !resolved.Info.RouterInstalled || strings.TrimSpace(resolved.Info.CAFingerprint) == "" ||
		strings.HasPrefix(resolved.Info.RouterInstanceID, "legacy-pid:") {
		_, err := resolved.Control.Bootstrap(ctx, true)
		if err != nil {
			return windowsCompanionUnavailableError(fmt.Errorf("Windows setup failed: %w", err))
		}
	}
	resolved, err = resolveWindowsCompanion(ctx, "control.bootstrap", "control.ca-certificate")
	if err != nil {
		return err
	}
	if strings.TrimSpace(resolved.Info.CAFingerprint) == "" {
		return windowsCompanionUnavailableError(errors.New("Windows setup completed without a certificate authority"))
	}
	certificate, err := resolved.Control.CACertificate(ctx)
	if err != nil {
		return windowsCompanionUnavailableError(err)
	}
	executable, err := wslSetupExecutableFunc()
	if err != nil {
		return err
	}
	linuxUser := ""
	if current, userErr := user.Current(); userErr == nil {
		linuxUser = current.Username
	}
	return installWSLIntegration(ctx, resolved.Info, certificate, wslIntegrationSetupConfig{
		StateDir:          router.DefaultStateDir(),
		CurrentExecutable: executable,
		Distro:            os.Getenv("WSL_DISTRO_NAME"),
		LinuxUser:         linuxUser,
		Runner:            wslSetupRunner,
		Input:             promptInput,
		Output:            os.Stderr,
	})
}

func promptAndSetupWSLAuthority(ctx context.Context, output io.Writer) error {
	if output == nil {
		output = io.Discard
	}
	fmt.Fprint(output, "gohere needs one-time setup for HTTPS .localhost URLs. sudo access may be requested.\n\nContinue? [Y/n] ")
	answer, readErr := bufio.NewReader(promptInput).ReadString('\n')
	if readErr != nil && strings.TrimSpace(answer) == "" {
		fmt.Fprintln(output, "gohere setup was not enabled. Run gohere setup when you are ready.")
		return errors.New("gohere setup was not enabled")
	}
	if !shouldRunSetupFromAnswer(answer) {
		fmt.Fprintln(output, "gohere setup was not enabled. Run gohere setup when you are ready.")
		return errors.New("gohere setup was not enabled")
	}
	return setupWSLAuthority(ctx)
}

func ensureWSLRunIdentity(ctx context.Context, info companion.Info, output io.Writer) (wslRunIdentity, error) {
	metadata, err := loadWSLIntegrationMetadata(router.DefaultStateDir())
	if err != nil || !metadataMatchesWindowsAuthority(metadata, info) {
		if err := promptAndSetupWSLAuthority(ctx, output); err != nil {
			return wslRunIdentity{}, err
		}
		metadata, err = loadWSLIntegrationMetadata(router.DefaultStateDir())
		if err != nil {
			return wslRunIdentity{}, fmt.Errorf("WSL setup completed without integration metadata: %w", err)
		}
		if !metadataMatchesWindowsAuthority(metadata, info) {
			return wslRunIdentity{}, errors.New("WSL integration metadata does not match the Windows authority")
		}
	}
	runnerID, err := randomIdentifier()
	if err != nil {
		return wslRunIdentity{}, err
	}
	return wslRunIdentity{
		OwnerInstance: metadata.OwnerInstance,
		Distro:        metadata.Distro,
		LinuxUser:     metadata.LinuxUser,
		RunnerID:      runnerID,
	}, nil
}

func loadWSLIntegrationMetadata(stateDir string) (wslIntegrationMetadata, error) {
	data, err := os.ReadFile(filepath.Join(stateDir, wslIntegrationDirname, "metadata.json"))
	if err != nil {
		return wslIntegrationMetadata{}, err
	}
	var metadata wslIntegrationMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return wslIntegrationMetadata{}, err
	}
	if metadata.ProtocolVersion != companion.ProtocolVersion || metadata.OwnerInstance == "" {
		return wslIntegrationMetadata{}, errors.New("WSL integration metadata is incompatible")
	}
	return metadata, nil
}

func metadataMatchesWindowsAuthority(metadata wslIntegrationMetadata, info companion.Info) bool {
	return metadata.ProtocolVersion == companion.ProtocolVersion &&
		metadata.OwnerInstance != "" &&
		metadata.CompanionVersion == info.CompanionVersion &&
		filepath.IsAbs(metadata.EdgeBinary) &&
		len(metadata.EdgeSHA256) == sha256.Size*2 &&
		strings.EqualFold(metadata.CAFingerprint, info.CAFingerprint) &&
		strings.EqualFold(metadata.WindowsStateDir, info.StateDir) &&
		metadata.WindowsUser == info.User
}

func applyWSLTrustEnvironment(environment []string, stateDir string) []string {
	values := map[string]string{
		"NODE_EXTRA_CA_CERTS": filepath.Join(stateDir, wslIntegrationDirname, "windows-ca.pem"),
	}
	for _, candidate := range []string{
		"/etc/ssl/certs/ca-certificates.crt",
		"/etc/pki/tls/certs/ca-bundle.crt",
		"/etc/ssl/ca-bundle.pem",
	} {
		if exists(candidate) {
			values["REQUESTS_CA_BUNDLE"] = candidate
			break
		}
	}
	return appendMissingEnv(environment, values)
}

func installWSLIntegration(ctx context.Context, info companion.Info, certificate string, cfg wslIntegrationSetupConfig) error {
	if cfg.StateDir == "" {
		return errors.New("WSL integration state directory is required")
	}
	if cfg.CurrentExecutable == "" {
		return errors.New("WSL executable path is required")
	}
	if cfg.Runner == nil {
		return errors.New("WSL setup command runner is required")
	}
	if cfg.Output == nil {
		cfg.Output = io.Discard
	}
	fingerprint, err := certificateFingerprint(certificate)
	if err != nil {
		return err
	}
	if !strings.EqualFold(fingerprint, strings.TrimSpace(info.CAFingerprint)) {
		return fmt.Errorf("Windows CA fingerprint mismatch: companion reported %s, certificate is %s", info.CAFingerprint, fingerprint)
	}

	integrationDir := filepath.Join(cfg.StateDir, wslIntegrationDirname)
	caPath := filepath.Join(integrationDir, "windows-ca.pem")
	if existing, readErr := os.ReadFile(caPath); readErr == nil && string(existing) != certificate {
		if !confirmWSLCAReplacement(cfg.Input, cfg.Output) {
			return errors.New("Windows CA replacement was not approved")
		}
	}
	if err := writeAtomicFile(caPath, []byte(certificate), 0644); err != nil {
		return err
	}

	edgeBinary, edgeHash, err := stageWSLEdgeBinary(cfg.CurrentExecutable, integrationDir)
	if err != nil {
		return err
	}
	fmt.Fprintln(cfg.Output, "Setting up gohere...")
	if err := cfg.Runner.Run(ctx, "sudo", "setcap", "cap_net_bind_service=+ep", edgeBinary); err != nil {
		return err
	}
	if err := certtrust.TrustCA(ctx, "linux", cfg.Runner, caPath); err != nil {
		return err
	}

	ownerInstance, err := ensureOwnerInstance(filepath.Join(integrationDir, "owner-instance"))
	if err != nil {
		return err
	}
	metadata := wslIntegrationMetadata{
		ProtocolVersion:  companion.ProtocolVersion,
		CompanionVersion: info.CompanionVersion,
		EdgeBinary:       edgeBinary,
		EdgeSHA256:       edgeHash,
		WindowsUser:      info.User,
		WindowsStateDir:  info.StateDir,
		RouterInstanceID: info.RouterInstanceID,
		CAFingerprint:    fingerprint,
		Distro:           cfg.Distro,
		LinuxUser:        cfg.LinuxUser,
		OwnerInstance:    ownerInstance,
	}
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeAtomicFile(filepath.Join(integrationDir, "metadata.json"), data, 0600)
}

type quietWSLSetupRunner struct{}

func (quietWSLSetupRunner) Run(ctx context.Context, command string, args ...string) error {
	cmd := execCommandContext(ctx, command, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = io.Discard
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func certificateFingerprint(certificate string) (string, error) {
	block, rest := pem.Decode([]byte(certificate))
	if block == nil || block.Type != "CERTIFICATE" {
		return "", errors.New("Windows companion returned an invalid CA certificate")
	}
	if len(strings.TrimSpace(string(rest))) != 0 {
		return "", errors.New("Windows companion returned unexpected data after the CA certificate")
	}
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", err
	}
	if !parsed.IsCA {
		return "", errors.New("Windows companion certificate is not a certificate authority")
	}
	sum := sha256.Sum256(parsed.Raw)
	return hex.EncodeToString(sum[:]), nil
}

func confirmWSLCAReplacement(input io.Reader, output io.Writer) bool {
	if input == nil {
		return false
	}
	fmt.Fprint(output, "The Windows gohere certificate authority changed. Replace the trusted WSL certificate? [y/N] ")
	answer, _ := bufio.NewReader(input).ReadString('\n')
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}

func ensureOwnerInstance(path string) (string, error) {
	if data, err := os.ReadFile(path); err == nil {
		value := strings.TrimSpace(string(data))
		if decoded, decodeErr := hex.DecodeString(value); decodeErr == nil && len(decoded) == 16 {
			return value, nil
		}
	}
	value, err := randomIdentifier()
	if err != nil {
		return "", err
	}
	if err := writeAtomicFile(path, []byte(value+"\n"), 0600); err != nil {
		return "", err
	}
	return value, nil
}

func randomIdentifier() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return hex.EncodeToString(random), nil
}

func stageWSLEdgeBinary(source, integrationDir string) (string, string, error) {
	hash, err := fileSHA256Hex(source)
	if err != nil {
		return "", "", err
	}
	destination := filepath.Join(integrationDir, "bin", wslEdgeBinaryName+"-"+hash)
	if existingHash, err := fileSHA256Hex(destination); err == nil && existingHash == hash {
		if err := os.Chmod(destination, 0755); err != nil {
			return "", "", err
		}
		return destination, hash, nil
	}
	if err := copyAtomicFile(source, destination, 0755); err != nil {
		return "", "", err
	}
	stagedHash, err := fileSHA256Hex(destination)
	if err != nil {
		return "", "", err
	}
	if stagedHash != hash {
		return "", "", errors.New("staged WSL edge hash does not match the current executable")
	}
	return destination, hash, nil
}

func fileSHA256Hex(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func copyAtomicFile(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
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
	if err := temp.Chmod(mode); err != nil {
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
	return os.Chmod(destination, mode)
}

func writeAtomicFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
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
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Chmod(mode); err != nil {
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
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	cleanup = false
	return os.Chmod(path, mode)
}
