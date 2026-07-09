package cert

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

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

func TestEnsureHostCertSupportsNestedLocalhost(t *testing.T) {
	store := Store{StateDir: t.TempDir()}

	tlsCert, err := store.EnsureHostCert("web.ctrltube.localhost")
	if err != nil {
		t.Fatal(err)
	}
	if len(tlsCert.Certificate) == 0 {
		t.Fatal("empty certificate chain")
	}

	leaf, err := x509.ParseCertificate(tlsCert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(leaf.DNSNames, "web.ctrltube.localhost") {
		t.Fatalf("DNSNames = %#v, want nested host", leaf.DNSNames)
	}

	certPath, keyPath, hostPath := store.HostCertPaths("web.ctrltube.localhost")
	for _, path := range []string{certPath, keyPath, hostPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected cached file %s: %v", path, err)
		}
	}
	if strings.Contains(filepath.Base(certPath), "web.ctrltube.localhost") {
		t.Fatalf("cert path = %q, want hashed filename", certPath)
	}
}

func TestEnsureHostCertSupportsLoopbackIP(t *testing.T) {
	store := Store{StateDir: t.TempDir()}

	tlsCert, err := store.EnsureHostCert("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(tlsCert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if !containsIP(leaf.IPAddresses, net.ParseIP("127.0.0.1")) {
		t.Fatalf("IPAddresses = %#v, want 127.0.0.1", leaf.IPAddresses)
	}
}

func TestEnsureHostCertRejectsNonLocalhostNames(t *testing.T) {
	store := Store{StateDir: t.TempDir()}

	_, err := store.EnsureHostCert("example.com")
	if err == nil {
		t.Fatal("expected invalid host error")
	}
	if !strings.Contains(err.Error(), "invalid certificate host") {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestEnsureHostCertReusesCachedCertificate(t *testing.T) {
	store := Store{StateDir: t.TempDir()}

	first, err := store.EnsureHostCert("app.localhost")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.EnsureHostCert("app.localhost")
	if err != nil {
		t.Fatal(err)
	}
	if string(first.Certificate[0]) != string(second.Certificate[0]) {
		t.Fatal("expected cached certificate to be reused")
	}
}

func TestPEMFilesContainCertificates(t *testing.T) {
	store := Store{StateDir: t.TempDir()}
	if _, err := store.EnsureHostCert("app.localhost"); err != nil {
		t.Fatal(err)
	}

	certPath, _, _ := store.HostCertPaths("app.localhost")
	data, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("PEM block = %#v, want CERTIFICATE", block)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsIP(values []net.IP, want net.IP) bool {
	for _, value := range values {
		if value.Equal(want) {
			return true
		}
	}
	return false
}
