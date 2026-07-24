package cert

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

const (
	caCertFilename               = "ca.pem"
	caKeyFilename                = "ca.key"
	fingerprintFilename          = "fingerprint"
	ephemeralHostCertValidity    = 24 * time.Hour
	caCertValidity               = 10 * 365 * 24 * time.Hour
	renewBefore                  = 30 * 24 * time.Hour
	certificateStoreLockTimeout  = 10 * time.Second
	certificateStoreLockPoll     = 10 * time.Millisecond
	certificateStoreLockFilename = "certificate-store.lock"
)

type Store struct {
	StateDir string
}

type CA struct {
	Cert        *x509.Certificate
	Key         *rsa.PrivateKey
	Fingerprint string
}

func (s Store) CACertPath() string {
	certPath, _, _ := s.caPaths()
	return certPath
}

func (s Store) EnsureCA() (CA, error) {
	unlock, err := s.lock("initialize certificate authority")
	if err != nil {
		return CA{}, err
	}
	defer unlock()

	certPath, keyPath, fingerprintPath := s.caPaths()
	now := time.Now()
	ca, err := loadValidatedCA(certPath, keyPath, now)
	if err == nil && ca.Cert.NotAfter.Sub(now) > renewBefore {
		fingerprint := fingerprint(ca.Cert.Raw)
		if err := writeFileIfChanged(fingerprintPath, []byte(fingerprint+"\n"), 0644); err != nil {
			return CA{}, err
		}
		ca.Fingerprint = fingerprint
		return ca, nil
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return CA{}, err
	}
	notBefore := now.Add(-time.Minute)
	template := &x509.Certificate{
		SerialNumber:          serialNumber(),
		Subject:               pkix.Name{CommonName: "gohere local development CA"},
		NotBefore:             notBefore,
		NotAfter:              notBefore.Add(caCertValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return CA{}, err
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return CA{}, err
	}
	if err := os.MkdirAll(filepath.Dir(certPath), 0700); err != nil {
		return CA{}, err
	}
	if err := writePEMFile(certPath, 0644, "CERTIFICATE", der); err != nil {
		return CA{}, err
	}
	if err := writePEMFile(keyPath, 0600, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(key)); err != nil {
		return CA{}, err
	}
	fp := fingerprint(der)
	if err := writeFileIfChanged(fingerprintPath, []byte(fp+"\n"), 0644); err != nil {
		return CA{}, err
	}
	return CA{Cert: parsed, Key: key, Fingerprint: fp}, nil
}

func (s Store) LoadCA() (CA, error) {
	unlock, err := s.lock("load certificate authority")
	if err != nil {
		return CA{}, err
	}
	defer unlock()
	return s.loadCALocked(time.Now())
}

func (s Store) Fingerprint() (string, error) {
	ca, err := s.LoadCA()
	if err != nil {
		return "", err
	}
	return ca.Fingerprint, nil
}

func (s Store) TrustFingerprint() (string, error) {
	ca, err := s.LoadCA()
	if err != nil {
		return "", err
	}
	sum := sha1.Sum(ca.Cert.Raw)
	return strings.ToUpper(hex.EncodeToString(sum[:])), nil
}

func (s Store) IssueEphemeralLANHostCert(host string, now time.Time) (tls.Certificate, error) {
	ca, err := s.LoadCA()
	if err != nil {
		return tls.Certificate{}, err
	}
	return s.IssueLANHostCert(ca, host, now)
}

func (s Store) IssueLANHostCert(ca CA, host string, now time.Time) (tls.Certificate, error) {
	canonical, err := canonicalLANHost(host)
	if err != nil {
		return tls.Certificate{}, err
	}
	return issueEphemeralHostCert(ca, canonical, now)
}

func (s Store) IssueLocalhostCert(ca CA, host string, now time.Time) (tls.Certificate, error) {
	canonical, err := canonicalLocalhostHost(host)
	if err != nil {
		return tls.Certificate{}, err
	}
	return issueEphemeralHostCert(ca, canonical, now)
}

func issueEphemeralHostCert(ca CA, canonical string, now time.Time) (tls.Certificate, error) {
	if ca.Cert == nil || ca.Key == nil {
		return tls.Certificate{}, errors.New("certificate authority is incomplete")
	}
	now = now.UTC()
	if now.Before(ca.Cert.NotBefore) || !now.Before(ca.Cert.NotAfter) {
		return tls.Certificate{}, errors.New("certificate authority is not currently valid")
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, err
	}
	notBefore := now.Add(-time.Minute)
	if ca.Cert.NotBefore.After(notBefore) {
		notBefore = ca.Cert.NotBefore
	}
	notAfter := notBefore.Add(ephemeralHostCertValidity)
	if ca.Cert.NotAfter.Before(notAfter) {
		notAfter = ca.Cert.NotAfter
	}
	if !notAfter.After(notBefore) {
		return tls.Certificate{}, errors.New("certificate authority validity is too short")
	}
	template := &x509.Certificate{
		SerialNumber: serialNumber(), Subject: pkix.Name{CommonName: canonical},
		NotBefore: notBefore, NotAfter: notAfter,
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{canonical},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return tls.Certificate{}, err
	}
	certificatePEM := append(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Cert.Raw})...,
	)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	certificate, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, err
	}
	certificate.Leaf, err = x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return certificate, nil
}

func canonicalLocalhostHost(host string) (string, error) {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" || strings.Contains(host, ":") || net.ParseIP(host) != nil {
		return "", fmt.Errorf("invalid certificate host %q", host)
	}
	if host != "localhost" && !strings.HasSuffix(host, ".localhost") {
		return "", fmt.Errorf("invalid certificate host %q: expected localhost or .localhost", host)
	}
	if !validLocalhostName(host) {
		return "", fmt.Errorf("invalid certificate host %q", host)
	}
	return host, nil
}

func canonicalLANHost(host string) (string, error) {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	label, ok := strings.CutSuffix(host, ".local")
	if !ok || label == "" || len(label) > 63 || strings.Contains(label, ".") || label[0] == '-' || label[len(label)-1] == '-' {
		return "", fmt.Errorf("invalid LAN hostname %q", host)
	}
	for _, character := range label {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return "", fmt.Errorf("invalid LAN hostname %q", host)
		}
	}
	return label + ".local", nil
}

func (s Store) caPaths() (certPath, keyPath, fingerprintPath string) {
	base := filepath.Join(s.StateDir, "ca")
	return filepath.Join(base, caCertFilename), filepath.Join(base, caKeyFilename), filepath.Join(base, fingerprintFilename)
}

func (s Store) lock(operation string) (func(), error) {
	if strings.TrimSpace(s.StateDir) == "" {
		return nil, errors.New("certificate state directory is required")
	}
	if err := os.MkdirAll(s.StateDir, 0700); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), certificateStoreLockTimeout)
	defer cancel()
	lockPath := filepath.Join(s.StateDir, certificateStoreLockFilename)
	fileLock := flock.New(lockPath, flock.SetPermissions(0600))
	locked, err := fileLock.TryLockContext(ctx, certificateStoreLockPoll)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", operation, err)
	}
	if !locked {
		return nil, fmt.Errorf("%s: timed out waiting for certificate store lock", operation)
	}
	if err := os.Chmod(lockPath, 0600); err != nil {
		_ = fileLock.Unlock()
		return nil, fmt.Errorf("%s: %w", operation, err)
	}
	return func() { _ = fileLock.Unlock() }, nil
}

func (s Store) loadCALocked(now time.Time) (CA, error) {
	certPath, keyPath, fingerprintPath := s.caPaths()
	ca, err := loadValidatedCA(certPath, keyPath, now)
	if err != nil {
		return CA{}, err
	}
	storedFingerprint, err := os.ReadFile(fingerprintPath)
	if err != nil {
		return CA{}, err
	}
	if strings.TrimSpace(string(storedFingerprint)) != ca.Fingerprint {
		return CA{}, errors.New("certificate authority fingerprint does not match certificate")
	}
	return ca, nil
}

func loadValidatedCA(certPath, keyPath string, now time.Time) (CA, error) {
	certDER, err := readSinglePEMBlock(certPath, "CERTIFICATE")
	if err != nil {
		return CA{}, err
	}
	keyDER, err := readSinglePEMBlock(keyPath, "RSA PRIVATE KEY")
	if err != nil {
		return CA{}, err
	}
	return validateCA(certDER, keyDER, now)
}

func validateCA(certDER, keyDER []byte, now time.Time) (CA, error) {
	parsedCert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return CA{}, err
	}
	parsedKey, err := x509.ParsePKCS1PrivateKey(keyDER)
	if err != nil {
		return CA{}, err
	}
	if !parsedCert.IsCA ||
		!parsedCert.BasicConstraintsValid ||
		parsedCert.KeyUsage&x509.KeyUsageCertSign == 0 {
		return CA{}, errors.New("invalid certificate authority constraints")
	}
	if now.Before(parsedCert.NotBefore) || !now.Before(parsedCert.NotAfter) {
		return CA{}, errors.New("certificate authority is not currently valid")
	}
	publicKey, ok := parsedCert.PublicKey.(*rsa.PublicKey)
	if !ok || !publicKey.Equal(&parsedKey.PublicKey) {
		return CA{}, errors.New("certificate authority key does not match certificate")
	}
	if !bytes.Equal(parsedCert.RawIssuer, parsedCert.RawSubject) {
		return CA{}, errors.New("certificate authority is not self-issued")
	}
	if err := parsedCert.CheckSignatureFrom(parsedCert); err != nil {
		return CA{}, fmt.Errorf("certificate authority is not self-signed: %w", err)
	}
	return CA{Cert: parsedCert, Key: parsedKey, Fingerprint: fingerprint(parsedCert.Raw)}, nil
}

func (s Store) PruneLegacyHostCerts() error {
	unlock, err := s.lock("remove legacy host certificates")
	if err != nil {
		return err
	}
	defer unlock()

	root, err := os.OpenRoot(s.StateDir)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.RemoveAll(filepath.Join("tls", "certs")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func validLocalhostName(host string) bool {
	if len(host) > 253 {
		return false
	}
	labels := strings.Split(host, ".")
	for _, label := range labels {
		if label == "" ||
			len(label) > 63 ||
			strings.HasPrefix(label, "-") ||
			strings.HasSuffix(label, "-") {
			return false
		}
		for _, r := range label {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
				continue
			}
			return false
		}
	}
	return true
}

func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

func serialNumber() *big.Int {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return big.NewInt(time.Now().UnixNano())
	}
	return n
}

func readSinglePEMBlock(path, blockType string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != blockType {
		return nil, fmt.Errorf("expected one %s PEM block in %s", blockType, path)
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("unexpected data after %s PEM block in %s", blockType, path)
	}
	return block.Bytes, nil
}

func writePEMFile(path string, mode os.FileMode, blockType string, der []byte) error {
	return writeRawFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), mode)
}

func writeFileIfChanged(path string, data []byte, mode os.FileMode) error {
	if existing, err := os.ReadFile(path); err == nil && string(existing) == string(data) {
		return os.Chmod(path, mode)
	}
	return writeRawFile(path, data, mode)
}

func writeRawFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	if err := os.Chmod(path, mode); err != nil {
		return err
	}
	cleanup = false
	return nil
}
