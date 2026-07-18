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
	for _, want := range []string{"https://shop.local", "AA:BB:CC", "Only install this certificate on devices you control", "gohere-root.pem", "gohere.mobileconfig"} {
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
