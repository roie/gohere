package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLANTrustEndpointServesOnlyPublicBootstrapMaterial(t *testing.T) {
	server := NewServer(Config{Store: NewMemoryStore()})
	server.ConfigureLANTrust(LANTrustSession{
		Address: "192.168.1.42", Token: "secret-token", Hostname: "shop.local.",
		Fingerprint: "AA:BB:CC", CACertificatePEM: []byte("-----BEGIN CERTIFICATE-----\nPUBLIC\n-----END CERTIFICATE-----\n"),
	})

	page := lanTrustRequest(server, "192.168.1.42", "/__gohere/trust/secret-token")
	if page.Code != http.StatusOK {
		t.Fatalf("page status = %d, body = %q", page.Code, page.Body.String())
	}
	for _, want := range []string{
		"https://shop.local", "AA:BB:CC", "Only install this certificate on devices you control",
		"gohere-root.pem", "gohere.mobileconfig", "VPN &amp; Device Management",
		"Certificate Trust Settings", "Install a certificate", "Open shop.local",
		`fetch(appURL, {mode: "no-cors", cache: "no-store"})`,
	} {
		if !strings.Contains(page.Body.String(), want) {
			t.Fatalf("page missing %q", want)
		}
	}
	if strings.Contains(page.Body.String(), "PRIVATE KEY") {
		t.Fatal("page exposed private key material")
	}

	certificate := lanTrustRequest(server, "192.168.1.42", "/__gohere/trust/secret-token/gohere-root.pem")
	if certificate.Code != http.StatusOK || certificate.Body.String() != "-----BEGIN CERTIFICATE-----\nPUBLIC\n-----END CERTIFICATE-----\n" {
		t.Fatalf("certificate = %d %q", certificate.Code, certificate.Body.String())
	}
	if disposition := certificate.Header().Get("Content-Disposition"); !strings.Contains(disposition, "gohere-root.pem") {
		t.Fatalf("Content-Disposition = %q", disposition)
	}

	profile := lanTrustRequest(server, "192.168.1.42", "/__gohere/trust/secret-token/gohere.mobileconfig")
	if profile.Code != http.StatusOK || !strings.Contains(profile.Body.String(), "com.apple.security.root") {
		t.Fatalf("profile = %d %q", profile.Code, profile.Body.String())
	}
}

func TestLANTrustEndpointUsesCompactBootstrapPath(t *testing.T) {
	server := NewServer(Config{Store: NewMemoryStore()})
	server.ConfigureLANTrust(LANTrustSession{Address: "192.168.1.42", Token: "secret-token", Hostname: "shop.local.", CACertificatePEM: []byte("public")})
	if response := lanTrustRequest(server, "192.168.1.42", "/g/secret-token"); response.Code != http.StatusOK {
		t.Fatalf("compact path status = %d", response.Code)
	}
}

func TestLANTrustEndpointRejectsWrongHostTokenAndExpiredSession(t *testing.T) {
	server := NewServer(Config{Store: NewMemoryStore()})
	server.ConfigureLANTrust(LANTrustSession{Address: "192.168.1.42", Token: "secret-token", Hostname: "shop.local.", CACertificatePEM: []byte("public")})
	for _, test := range []struct {
		host string
		path string
	}{
		{host: "shop.local", path: "/__gohere/trust/secret-token"},
		{host: "192.168.1.42", path: "/__gohere/trust/wrong-token"},
		{host: "192.168.1.43", path: "/__gohere/trust/secret-token"},
	} {
		response := lanTrustRequest(server, test.host, test.path)
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s%s status = %d", test.host, test.path, response.Code)
		}
	}
	server.ClearLANTrust()
	if response := lanTrustRequest(server, "192.168.1.42", "/__gohere/trust/secret-token"); response.Code != http.StatusNotFound {
		t.Fatalf("expired status = %d", response.Code)
	}
}

func TestLANTrustEndpointRejectsSessionFromPreviousCAImmediately(t *testing.T) {
	server := NewServer(Config{Store: NewMemoryStore()})
	server.currentLANCAFingerprint = func() (string, error) {
		return "current-ca", nil
	}
	server.ConfigureLANTrust(LANTrustSession{
		Address: "192.168.1.42", Token: "secret-token", Hostname: "shop.local.",
		CAFingerprint: "previous-ca", CACertificatePEM: []byte("previous public CA"),
	})

	response := lanTrustRequest(server, "192.168.1.42", "/g/secret-token/gohere-root.pem")
	if response.Code != http.StatusNotFound {
		t.Fatalf("stale trust status = %d, body = %q", response.Code, response.Body.String())
	}
	server.lanTrustMu.Lock()
	_, exists := server.lanTrustSessions["secret-token"]
	server.lanTrustMu.Unlock()
	if exists {
		t.Fatal("stale trust session was not invalidated synchronously")
	}
}

func TestLANTrustEndpointRateLimitsTokenGuesses(t *testing.T) {
	server := NewServer(Config{Store: NewMemoryStore()})
	server.ConfigureLANTrust(LANTrustSession{Address: "192.168.1.42", Token: "secret-token", Hostname: "shop.local.", CACertificatePEM: []byte("public")})
	for attempt := 0; attempt < 20; attempt++ {
		response := lanTrustRequest(server, "192.168.1.42", "/__gohere/trust/wrong-token")
		if response.Code != http.StatusNotFound {
			t.Fatalf("attempt %d status = %d", attempt, response.Code)
		}
	}
	if response := lanTrustRequest(server, "192.168.1.42", "/__gohere/trust/wrong-token"); response.Code != http.StatusTooManyRequests {
		t.Fatalf("rate-limited status = %d", response.Code)
	}
}

func lanTrustRequest(server *Server, host, path string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "http://"+host+path, nil)
	request.Host = host
	request.RemoteAddr = "192.168.1.58:43210"
	response := httptest.NewRecorder()
	server.LANHTTPHandler().ServeHTTP(response, request)
	return response
}

func TestLANTrustPageShowsOnlyTheDetectedPlatformSteps(t *testing.T) {
	server := NewServer(Config{Store: NewMemoryStore()})
	server.ConfigureLANTrust(LANTrustSession{
		Address: "192.168.1.42", Token: "secret-token", Hostname: "shop.local.",
		Fingerprint: "AA:BB:CC", CACertificatePEM: []byte("-----BEGIN CERTIFICATE-----\nPUBLIC\n-----END CERTIFICATE-----\n"),
	})
	for _, test := range []struct {
		name, userAgent string
		apple, android  bool
	}{
		{"iPhone", "Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Mobile/15E148 Safari/604.1", true, false},
		{"iPad", "Mozilla/5.0 (iPad; CPU OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Mobile/15E148 Safari/604.1", true, false},
		{"Android", "Mozilla/5.0 (Linux; Android 15; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Mobile Safari/537.36", false, true},
		{"unknown", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Safari/605.1.15", true, true},
		{"missing", "", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://192.168.1.42/__gohere/trust/secret-token", nil)
			request.Host = "192.168.1.42"
			request.RemoteAddr = "192.168.1.58:43210"
			request.Header.Set("User-Agent", test.userAgent)
			response := httptest.NewRecorder()
			server.LANHTTPHandler().ServeHTTP(response, request)
			body := response.Body.String()
			if response.Code != http.StatusOK || !strings.Contains(body, "AA:BB:CC") || !strings.Contains(body, "Open shop.local") {
				t.Fatalf("page = %d %q", response.Code, body)
			}
			if got := strings.Contains(body, "gohere.mobileconfig") && strings.Contains(body, "Certificate Trust Settings"); got != test.apple {
				t.Fatalf("Apple steps shown = %v, want %v", got, test.apple)
			}
			if got := strings.Contains(body, "Install a certificate"); got != test.android {
				t.Fatalf("Android steps shown = %v, want %v", got, test.android)
			}
			if got := strings.Contains(body, "<h2>"); got != (test.apple && test.android) {
				t.Fatalf("platform headings shown = %v, want only when both platforms are shown", got)
			}
		})
	}
}
