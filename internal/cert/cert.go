package cert

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	caCertFilename      = "ca.pem"
	caKeyFilename       = "ca.key"
	fingerprintFilename = "fingerprint"
	hostCertValidity    = 397 * 24 * time.Hour
	caCertValidity      = 10 * 365 * 24 * time.Hour
	renewBefore         = 30 * 24 * time.Hour
)

type Store struct {
	StateDir string
}

type CA struct {
	Cert        *x509.Certificate
	Key         *rsa.PrivateKey
	Fingerprint string
}

func (s Store) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			host := "localhost"
			if hello != nil && hello.ServerName != "" {
				host = hello.ServerName
			}
			cert, err := s.EnsureHostCert(host)
			if err != nil {
				return nil, err
			}
			return &cert, nil
		},
	}
}

func (s Store) CACertPath() string {
	certPath, _, _ := s.caPaths()
	return certPath
}

func (s Store) EnsureCA() (CA, error) {
	certPath, keyPath, fingerprintPath := s.caPaths()
	ca, err := s.loadCA(certPath, keyPath)
	if err == nil && time.Until(ca.Cert.NotAfter) > renewBefore {
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
	now := time.Now().Add(-time.Minute)
	template := &x509.Certificate{
		SerialNumber:          serialNumber(),
		Subject:               pkix.Name{CommonName: "gohere local development CA"},
		NotBefore:             now,
		NotAfter:              now.Add(caCertValidity),
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

func (s Store) Fingerprint() (string, error) {
	_, _, fingerprintPath := s.caPaths()
	data, err := os.ReadFile(fingerprintPath)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func (s Store) TrustFingerprint() (string, error) {
	certPath, _, _ := s.caPaths()
	der, err := readPEMBlock(certPath, "CERTIFICATE")
	if err != nil {
		return "", err
	}
	sum := sha1.Sum(der)
	return strings.ToUpper(hex.EncodeToString(sum[:])), nil
}

func (s Store) EnsureHostCert(host string) (tls.Certificate, error) {
	canonical, parsedIP, err := canonicalHost(host)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPath, keyPath, hostPath := s.HostCertPaths(canonical)
	if cert, ok := loadReusableHostCert(certPath, keyPath, canonical); ok {
		return cert, nil
	}

	ca, err := s.EnsureCA()
	if err != nil {
		return tls.Certificate{}, err
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now().Add(-time.Minute)
	template := &x509.Certificate{
		SerialNumber: serialNumber(),
		Subject:      pkix.Name{CommonName: canonical},
		NotBefore:    now,
		NotAfter:     now.Add(hostCertValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if parsedIP != nil {
		template.IPAddresses = []net.IP{parsedIP}
	} else {
		template.DNSNames = []string{canonical}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := os.MkdirAll(filepath.Dir(certPath), 0700); err != nil {
		return tls.Certificate{}, err
	}
	certPEM := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Cert.Raw})...)
	if err := writeRawFile(certPath, certPEM, 0644); err != nil {
		return tls.Certificate{}, err
	}
	if err := writePEMFile(keyPath, 0600, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(key)); err != nil {
		return tls.Certificate{}, err
	}
	if err := writeFileIfChanged(hostPath, []byte(canonical+"\n"), 0644); err != nil {
		return tls.Certificate{}, err
	}
	return tls.LoadX509KeyPair(certPath, keyPath)
}

func (s Store) HostCertPaths(host string) (certPath, keyPath, hostPath string) {
	key := hostCacheKey(host)
	base := filepath.Join(s.StateDir, "tls", "certs", key)
	return base + ".pem", base + ".key", base + ".host"
}

func (s Store) caPaths() (certPath, keyPath, fingerprintPath string) {
	base := filepath.Join(s.StateDir, "ca")
	return filepath.Join(base, caCertFilename), filepath.Join(base, caKeyFilename), filepath.Join(base, fingerprintFilename)
}

func (s Store) loadCA(certPath, keyPath string) (CA, error) {
	certDER, err := readPEMBlock(certPath, "CERTIFICATE")
	if err != nil {
		return CA{}, err
	}
	keyDER, err := readPEMBlock(keyPath, "RSA PRIVATE KEY")
	if err != nil {
		return CA{}, err
	}
	parsedCert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return CA{}, err
	}
	parsedKey, err := x509.ParsePKCS1PrivateKey(keyDER)
	if err != nil {
		return CA{}, err
	}
	return CA{Cert: parsedCert, Key: parsedKey, Fingerprint: fingerprint(parsedCert.Raw)}, nil
}

func loadReusableHostCert(certPath, keyPath, wantHost string) (tls.Certificate, bool) {
	tlsCert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil || len(tlsCert.Certificate) == 0 {
		return tls.Certificate{}, false
	}
	leaf, err := x509.ParseCertificate(tlsCert.Certificate[0])
	if err != nil || time.Until(leaf.NotAfter) <= renewBefore {
		return tls.Certificate{}, false
	}
	if ip := net.ParseIP(wantHost); ip != nil {
		for _, certIP := range leaf.IPAddresses {
			if certIP.Equal(ip) {
				return tlsCert, true
			}
		}
		return tls.Certificate{}, false
	}
	for _, name := range leaf.DNSNames {
		if name == wantHost {
			return tlsCert, true
		}
	}
	return tls.Certificate{}, false
}

func canonicalHost(host string) (string, net.IP, error) {
	host = strings.TrimSpace(strings.ToLower(host))
	if strings.Contains(host, ":") {
		if splitHost, _, err := net.SplitHostPort(host); err == nil {
			host = strings.Trim(splitHost, "[]")
		}
	}
	if host == "" {
		return "", nil, fmt.Errorf("invalid certificate host %q", host)
	}
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return "", nil, fmt.Errorf("invalid certificate host %q: only loopback IPs are supported", host)
		}
		return ip.String(), ip, nil
	}
	if host != "localhost" && !strings.HasSuffix(host, ".localhost") {
		return "", nil, fmt.Errorf("invalid certificate host %q: expected localhost or .localhost", host)
	}
	if !validLocalhostName(host) {
		return "", nil, fmt.Errorf("invalid certificate host %q", host)
	}
	return host, nil, nil
}

func validLocalhostName(host string) bool {
	labels := strings.Split(host, ".")
	for _, label := range labels {
		if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
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

func hostCacheKey(host string) string {
	canonical, _, err := canonicalHost(host)
	if err != nil {
		canonical = strings.TrimSpace(strings.ToLower(host))
	}
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
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

func readPEMBlock(path, blockType string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	for {
		block, rest := pem.Decode(data)
		if block == nil {
			return nil, fmt.Errorf("missing %s PEM block in %s", blockType, path)
		}
		if block.Type == blockType {
			return block.Bytes, nil
		}
		data = rest
	}
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
