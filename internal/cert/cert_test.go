package cert

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLoadCARejectsInconsistentState(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, store Store)
	}{
		{
			name: "missing key",
			mutate: func(t *testing.T, store Store) {
				t.Helper()
				if err := os.Remove(filepath.Join(store.StateDir, "ca", caKeyFilename)); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "mismatched key",
			mutate: func(t *testing.T, store Store) {
				t.Helper()
				other := Store{StateDir: t.TempDir()}
				if _, err := other.EnsureCA(); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(filepath.Join(other.StateDir, "ca", caKeyFilename))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(store.StateDir, "ca", caKeyFilename), data, 0600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "malformed certificate",
			mutate: func(t *testing.T, store Store) {
				t.Helper()
				if err := os.WriteFile(
					filepath.Join(store.StateDir, "ca", caCertFilename),
					[]byte("not a certificate"),
					0644,
				); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "additional certificate block",
			mutate: func(t *testing.T, store Store) {
				t.Helper()
				path := filepath.Join(store.StateDir, "ca", caCertFilename)
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append(data, data...), 0644); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := Store{StateDir: t.TempDir()}
			if _, err := store.EnsureCA(); err != nil {
				t.Fatal(err)
			}
			test.mutate(t, store)
			if _, err := store.LoadCA(); err == nil {
				t.Fatal("LoadCA accepted inconsistent state")
			}
		})
	}
}

func TestLoadCAReturnsExistingCAWithoutWriting(t *testing.T) {
	store := Store{StateDir: t.TempDir()}
	created, err := store.EnsureCA()
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotFilesBelow(t, store.StateDir)

	loaded, err := store.LoadCA()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Fingerprint != created.Fingerprint {
		t.Fatalf("fingerprint = %q, want %q", loaded.Fingerprint, created.Fingerprint)
	}
	after := snapshotFilesBelow(t, store.StateDir)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("LoadCA changed files: before=%v after=%v", before, after)
	}
}

func TestLoadCADoesNotRotateNearExpiry(t *testing.T) {
	store := Store{StateDir: t.TempDir()}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(42),
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(5 * time.Minute),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath, fingerprintPath := store.caPaths()
	if err := writePEMFile(certPath, 0644, "CERTIFICATE", der); err != nil {
		t.Fatal(err)
	}
	if err := writePEMFile(
		keyPath,
		0600,
		"RSA PRIVATE KEY",
		x509.MarshalPKCS1PrivateKey(key),
	); err != nil {
		t.Fatal(err)
	}
	if err := writeRawFile(
		fingerprintPath,
		[]byte(fingerprint(der)+"\n"),
		0644,
	); err != nil {
		t.Fatal(err)
	}
	before := make(map[string][]byte)
	for _, path := range []string{certPath, keyPath, fingerprintPath} {
		before[path], err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}

	loaded, err := store.LoadCA()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Fingerprint != fingerprint(der) {
		t.Fatalf("fingerprint = %q, want %q", loaded.Fingerprint, fingerprint(der))
	}
	for path, want := range before {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("LoadCA rewrote %s", path)
		}
	}
}

func TestLoadCARejectsFingerprintMismatch(t *testing.T) {
	store := Store{StateDir: t.TempDir()}
	if _, err := store.EnsureCA(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(store.StateDir, "ca", fingerprintFilename),
		[]byte("wrong\n"),
		0644,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadCA(); err == nil {
		t.Fatal("LoadCA accepted a mismatched fingerprint")
	}
}

func TestValidateCARejectsNonCAAndExpiredCertificates(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER := x509.MarshalPKCS1PrivateKey(key)
	if _, err := validateCA(der, keyDER, now); err == nil {
		t.Fatal("validateCA accepted a non-CA certificate")
	}

	template.IsCA = true
	template.BasicConstraintsValid = true
	template.KeyUsage = x509.KeyUsageCertSign
	der, err = x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateCA(der, keyDER, template.NotAfter); err == nil {
		t.Fatal("validateCA accepted an expired certificate")
	}
}

func TestValidateCARejectsCertificateSignedByAnotherAuthority(t *testing.T) {
	now := time.Now().UTC()
	rootKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rootTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	rootDER, err := x509.CreateCertificate(
		rand.Reader,
		rootTemplate,
		rootTemplate,
		&rootKey.PublicKey,
		rootKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}

	storedKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	storedTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	storedDER, err := x509.CreateCertificate(
		rand.Reader,
		storedTemplate,
		root,
		&storedKey.PublicKey,
		rootKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateCA(
		storedDER,
		x509.MarshalPKCS1PrivateKey(storedKey),
		now,
	); err == nil {
		t.Fatal("validateCA accepted a certificate signed by another authority")
	}
}

func TestValidateCARejectsSameKeyWithDifferentIssuer(t *testing.T) {
	now := time.Now().UTC()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	issuerTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "different issuer"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	issuerDER, err := x509.CreateCertificate(
		rand.Reader,
		issuerTemplate,
		issuerTemplate,
		&key.PublicKey,
		key,
	)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := x509.ParseCertificate(issuerDER)
	if err != nil {
		t.Fatal(err)
	}
	storedTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "stored authority"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	storedDER, err := x509.CreateCertificate(
		rand.Reader,
		storedTemplate,
		issuer,
		&key.PublicKey,
		key,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateCA(
		storedDER,
		x509.MarshalPKCS1PrivateKey(key),
		now,
	); err == nil {
		t.Fatal("validateCA accepted a same-key certificate with a different issuer")
	}
}

func TestEnsureCAConcurrentCallersShareOneFingerprint(t *testing.T) {
	store := Store{StateDir: t.TempDir()}
	const callers = 8
	start := make(chan struct{})
	results := make(chan CA, callers)
	errs := make(chan error, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			ca, err := store.EnsureCA()
			if err != nil {
				errs <- err
				return
			}
			results <- ca
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var fingerprint string
	for ca := range results {
		if fingerprint == "" {
			fingerprint = ca.Fingerprint
		}
		if ca.Fingerprint != fingerprint {
			t.Fatalf("fingerprint = %q, want %q", ca.Fingerprint, fingerprint)
		}
	}
	loaded, err := store.LoadCA()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Fingerprint != fingerprint {
		t.Fatalf("loaded fingerprint = %q, want %q", loaded.Fingerprint, fingerprint)
	}
}

func TestEnsureCAConcurrentProcessesShareOneFingerprint(t *testing.T) {
	const helperStateEnv = "GOHERE_CERT_CONCURRENCY_HELPER_STATE"
	if stateDir := os.Getenv(helperStateEnv); stateDir != "" {
		ca, err := (Store{StateDir: stateDir}).EnsureCA()
		if err != nil {
			t.Fatal(err)
		}
		fmt.Println(ca.Fingerprint)
		return
	}

	stateDir := t.TempDir()
	const processes = 6
	type process struct {
		command *exec.Cmd
		stdout  bytes.Buffer
		stderr  bytes.Buffer
	}
	children := make([]*process, processes)
	for index := range processes {
		child := &process{
			command: exec.Command(
				os.Args[0],
				"-test.run=^TestEnsureCAConcurrentProcessesShareOneFingerprint$",
			),
		}
		child.command.Env = append(os.Environ(), helperStateEnv+"="+stateDir)
		child.command.Stdout = &child.stdout
		child.command.Stderr = &child.stderr
		if err := child.command.Start(); err != nil {
			t.Fatal(err)
		}
		children[index] = child
	}

	fingerprints := make(map[string]struct{})
	for _, child := range children {
		if err := child.command.Wait(); err != nil {
			t.Fatalf("helper failed: %v\n%s", err, child.stderr.String())
		}
		fields := strings.Fields(child.stdout.String())
		if len(fields) == 0 {
			t.Fatalf("helper returned no fingerprint: %q", child.stdout.String())
		}
		fingerprints[fields[0]] = struct{}{}
	}
	if len(fingerprints) != 1 {
		t.Fatalf("concurrent processes produced fingerprints %#v", fingerprints)
	}
	if _, err := (Store{StateDir: stateDir}).LoadCA(); err != nil {
		t.Fatalf("final CA state is invalid: %v", err)
	}
}

func TestIssueLocalhostCertUsesCapturedCAWithoutPersistence(t *testing.T) {
	store := Store{StateDir: t.TempDir()}
	ca, err := store.EnsureCA()
	if err != nil {
		t.Fatal(err)
	}
	before := filesBelow(t, store.StateDir)
	now := time.Now().UTC()

	leaf, err := store.IssueLocalhostCert(ca, "Web.App.Localhost.", now)
	if err != nil {
		t.Fatal(err)
	}
	if leaf.Leaf == nil || len(leaf.Leaf.DNSNames) != 1 ||
		leaf.Leaf.DNSNames[0] != "web.app.localhost" {
		t.Fatalf("leaf = %#v", leaf.Leaf)
	}
	if leaf.Leaf.NotAfter.Sub(leaf.Leaf.NotBefore) != 24*time.Hour {
		t.Fatalf("validity = %v, want 24h", leaf.Leaf.NotAfter.Sub(leaf.Leaf.NotBefore))
	}
	if err := leaf.Leaf.CheckSignatureFrom(ca.Cert); err != nil {
		t.Fatal(err)
	}
	after := filesBelow(t, store.StateDir)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("ephemeral issuance changed files: before=%v after=%v", before, after)
	}
}

func TestIssueLocalhostCertDoesNotOutliveCapturedCA(t *testing.T) {
	now := time.Now().UTC()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(7),
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(5 * time.Minute),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	ca := CA{Cert: certificate, Key: key, Fingerprint: fingerprint(der)}

	leaf, err := (Store{}).IssueLocalhostCert(ca, "app.localhost", now)
	if err != nil {
		t.Fatal(err)
	}
	if leaf.Leaf.NotAfter.After(ca.Cert.NotAfter) {
		t.Fatalf("leaf expires at %s after CA expiry %s", leaf.Leaf.NotAfter, ca.Cert.NotAfter)
	}
}

func TestIssueLocalhostCertRejectsIPAndNonLocalhostNames(t *testing.T) {
	store := Store{StateDir: t.TempDir()}
	ca, err := store.EnsureCA()
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{
		"127.0.0.1",
		"::1",
		"example.com",
		"bad..localhost",
		strings.Repeat("a", 64) + ".localhost",
	} {
		t.Run(host, func(t *testing.T) {
			if _, err := store.IssueLocalhostCert(ca, host, time.Now()); err == nil {
				t.Fatalf("IssueLocalhostCert accepted %q", host)
			}
		})
	}
}

func TestPruneLegacyHostCertsDoesNotFollowSymlink(t *testing.T) {
	parent := t.TempDir()
	stateDir := filepath.Join(parent, "state")
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(filepath.Join(stateDir, "tls"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	outsideFile := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(outsideFile, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(stateDir, "tls", "certs")
	if err := os.Symlink(outside, legacyPath); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Fatal(err)
	}

	if err := (Store{StateDir: stateDir}).PruneLegacyHostCerts(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(outsideFile); err != nil {
		t.Fatalf("cleanup escaped state dir: %v", err)
	}
	if _, err := os.Lstat(legacyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy cert path still exists: %v", err)
	}
}

func TestIssueEphemeralLANHostCertUsesExistingCAWithoutPersistingLeaf(t *testing.T) {
	store := Store{StateDir: t.TempDir()}
	ca, err := store.EnsureCA()
	if err != nil {
		t.Fatal(err)
	}
	before := filesBelow(t, store.StateDir)
	now := time.Now().UTC()
	leaf, err := store.IssueEphemeralLANHostCert("Shop.Local.", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaf.Certificate) < 1 || leaf.PrivateKey == nil {
		t.Fatalf("certificate = %#v", leaf)
	}
	parsed, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.DNSNames) != 1 || parsed.DNSNames[0] != "shop.local" {
		t.Fatalf("DNS names = %#v", parsed.DNSNames)
	}
	if parsed.NotAfter.Sub(parsed.NotBefore) != 24*time.Hour {
		t.Fatalf("validity = %s", parsed.NotAfter.Sub(parsed.NotBefore))
	}
	if err := parsed.CheckSignatureFrom(ca.Cert); err != nil {
		t.Fatal(err)
	}
	after := filesBelow(t, store.StateDir)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("ephemeral leaf changed files: before %v, after %v", before, after)
	}
}

func TestIssueEphemeralLANHostCertRejectsNonLocalName(t *testing.T) {
	store := Store{StateDir: t.TempDir()}
	if _, err := store.EnsureCA(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.IssueEphemeralLANHostCert("shop.localhost", time.Now()); err == nil {
		t.Fatal("non-.local hostname accepted")
	}
}

type fileSnapshot struct {
	Data    string
	Mode    fs.FileMode
	ModTime time.Time
}

func snapshotFilesBelow(t *testing.T, root string) map[string]fileSnapshot {
	t.Helper()
	files := make(map[string]fileSnapshot)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[strings.TrimPrefix(path, root)] = fileSnapshot{
			Data: string(data), Mode: info.Mode(), ModTime: info.ModTime(),
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func filesBelow(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			files = append(files, strings.TrimPrefix(path, root))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	return files
}

func TestEnsureCAGeneratesAndPersistsFingerprint(t *testing.T) {
	store := Store{StateDir: t.TempDir()}

	ca, err := store.EnsureCA()
	if err != nil {
		t.Fatal(err)
	}
	if ca.Fingerprint == "" {
		t.Fatal("fingerprint is empty")
	}

	keyInfo, err := os.Stat(filepath.Join(store.StateDir, "ca", "ca.key"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && keyInfo.Mode().Perm() != 0600 {
		t.Fatalf("ca.key permissions = %v, want 0600", keyInfo.Mode().Perm())
	}

	got, err := store.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if got != ca.Fingerprint {
		t.Fatalf("fingerprint = %q, want %q", got, ca.Fingerprint)
	}
	trustFingerprint, err := store.TrustFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if len(trustFingerprint) != 40 || strings.ToUpper(trustFingerprint) != trustFingerprint {
		t.Fatalf("trust fingerprint = %q, want uppercase SHA-1 thumbprint", trustFingerprint)
	}
}
