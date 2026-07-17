package router

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func canonicalizationHandler(t *testing.T, route Route) http.Handler {
	t.Helper()
	store := NewMemoryStore()
	if err := store.Save([]Route{route}); err != nil {
		t.Fatal(err)
	}
	return NewServer(Config{Store: store}).HTTPHandler()
}

func TestHTTPSRouteRedirectsOrdinaryHTTPAuthoritatively(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			route := Route{
				Host:            "app.localhost",
				PreferredScheme: "https",
				Target:          "http://127.0.0.1:41001",
			}
			req := httptest.NewRequest(method, "http://app.localhost:8080/a%2Fb?token=secret", strings.NewReader("body"))
			req.Host = "app.localhost:8080"
			req.Header.Set("Forwarded", "proto=https;host=evil.example:9443")
			req.Header.Set("X-Forwarded-Proto", "https")
			req.Header.Set("X-Forwarded-Host", "evil.example:9443")
			rec := httptest.NewRecorder()
			canonicalizationHandler(t, route).ServeHTTP(rec, req)

			if rec.Code != http.StatusTemporaryRedirect {
				t.Fatalf("status/body = %d/%q", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Location"); got != "https://app.localhost/a%2Fb?token=secret" {
				t.Fatalf("Location = %q", got)
			}
			if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Content-Length") != "0" {
				t.Fatalf("headers = %#v", rec.Header())
			}
			if rec.Header().Get("Strict-Transport-Security") != "" || rec.Body.Len() != 0 {
				t.Fatalf("HSTS/body = %q/%q", rec.Header().Get("Strict-Transport-Security"), rec.Body.String())
			}
		})
	}
}

func TestHTTPSRedirectPreservesBareQuery(t *testing.T) {
	route := Route{Host: "app.localhost", PreferredScheme: "https", Target: "http://127.0.0.1:41001"}
	req := httptest.NewRequest(http.MethodGet, "http://app.localhost/path?", nil)
	req.Host = "app.localhost"
	rec := httptest.NewRecorder()
	canonicalizationHandler(t, route).ServeHTTP(rec, req)
	if got := rec.Header().Get("Location"); got != "https://app.localhost/path?" {
		t.Fatalf("Location = %q", got)
	}
}

func TestHTTPSRouteRejectsPreflightWithoutReflectingQuery(t *testing.T) {
	route := Route{Host: "app.localhost", PreferredScheme: "https", Target: "http://127.0.0.1:41001"}
	for _, accept := range []string{"application/json", "text/plain"} {
		t.Run(accept, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodOptions, "http://app.localhost/api%2Fv1?token=secret", nil)
			req.Host = "app.localhost"
			req.Header.Set("Origin", "https://client.localhost")
			req.Header.Set("Access-Control-Request-Method", "POST")
			req.Header.Set("Accept", accept)
			rec := httptest.NewRecorder()
			canonicalizationHandler(t, route).ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest || rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status/headers = %d/%#v", rec.Code, rec.Header())
			}
			if rec.Header().Get("Location") != "" || rec.Header().Get("Access-Control-Allow-Origin") != "" || rec.Header().Get("Strict-Transport-Security") != "" || strings.Contains(rec.Body.String(), "token=secret") {
				t.Fatalf("headers/body = %#v/%q", rec.Header(), rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "https://app.localhost/api%2Fv1") {
				t.Fatalf("body = %q", rec.Body.String())
			}
			if accept == "application/json" {
				var payload proxyErrorPayload
				if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				if payload.Error != "secure_transport_required" {
					t.Fatalf("payload = %#v", payload)
				}
			}
		})
	}
}

func TestHTTPSRouteRejectsWebSocketUpgradeAttemptByExactTokens(t *testing.T) {
	route := Route{Host: "app.localhost", PreferredScheme: "https", Target: "http://127.0.0.1:41001"}
	tests := []struct {
		name       string
		method     string
		upgrade    string
		connection string
		wantStatus int
	}{
		{"exact", http.MethodGet, "websocket", "upgrade", http.StatusBadRequest},
		{"lists-and-case", http.MethodGet, "h2c, WebSocket", "keep-alive, UpGrade", http.StatusBadRequest},
		{"upgrade-substring", http.MethodGet, "xwebsocket", "upgrade", http.StatusTemporaryRedirect},
		{"versioned-upgrade-token", http.MethodGet, "websocket/13", "upgrade", http.StatusTemporaryRedirect},
		{"connection-substring", http.MethodGet, "websocket", "upgrader", http.StatusTemporaryRedirect},
		{"missing-upgrade", http.MethodGet, "", "upgrade", http.StatusTemporaryRedirect},
		{"missing-connection", http.MethodGet, "websocket", "", http.StatusTemporaryRedirect},
		{"non-get", http.MethodPost, "websocket", "upgrade", http.StatusTemporaryRedirect},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(test.method, "http://app.localhost/socket?token=secret", nil)
			req.Host = "app.localhost"
			req.Header.Set("Upgrade", test.upgrade)
			req.Header.Set("Connection", test.connection)
			rec := httptest.NewRecorder()
			canonicalizationHandler(t, route).ServeHTTP(rec, req)
			if rec.Code != test.wantStatus {
				t.Fatalf("status/body = %d/%q", rec.Code, rec.Body.String())
			}
			if test.wantStatus == http.StatusBadRequest {
				if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Location") != "" || rec.Header().Get("Strict-Transport-Security") != "" {
					t.Fatalf("headers = %#v", rec.Header())
				}
				if !strings.Contains(rec.Body.String(), "wss://app.localhost/socket") || strings.Contains(rec.Body.String(), "token=secret") {
					t.Fatalf("body = %q", rec.Body.String())
				}
			}
		})
	}
}

func TestHTTPSRouteWebSocketRejectionNegotiatesJSON(t *testing.T) {
	route := Route{Host: "app.localhost", PreferredScheme: "https", Target: "http://127.0.0.1:41001"}
	req := httptest.NewRequest(http.MethodGet, "http://app.localhost/socket?token=secret", nil)
	req.Host = "app.localhost"
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	canonicalizationHandler(t, route).ServeHTTP(rec, req)
	var payload proxyErrorPayload
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest || payload.Error != "secure_websocket_required" || strings.Contains(payload.Message, "token=secret") {
		t.Fatalf("status/payload = %d/%#v", rec.Code, payload)
	}
}

func TestHTTPSRouteRedirectsOptionsThatIsNotPreflight(t *testing.T) {
	route := Route{Host: "app.localhost", PreferredScheme: "https", Target: "http://127.0.0.1:41001"}
	tests := []struct {
		name                 string
		originPresent        bool
		origin               string
		requestMethodPresent bool
		requestMethod        string
	}{
		{name: "missing-origin", requestMethodPresent: true, requestMethod: "POST"},
		{name: "empty-origin", originPresent: true, requestMethodPresent: true, requestMethod: "POST"},
		{name: "missing-request-method", originPresent: true, origin: "https://client.localhost"},
		{name: "empty-request-method", originPresent: true, origin: "https://client.localhost", requestMethodPresent: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodOptions, "http://app.localhost/resource", nil)
			req.Host = "app.localhost"
			if test.originPresent {
				req.Header.Set("Origin", test.origin)
			}
			if test.requestMethodPresent {
				req.Header.Set("Access-Control-Request-Method", test.requestMethod)
			}
			rec := httptest.NewRecorder()
			canonicalizationHandler(t, route).ServeHTTP(rec, req)
			if rec.Code != http.StatusTemporaryRedirect {
				t.Fatalf("status/body = %d/%q", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestRouteSchemeProxyMatrixPreservesUpstreamHSTS(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Strict-Transport-Security", "max-age=60")
		io.WriteString(w, "ok")
	}))
	defer backend.Close()

	tests := []struct {
		name       string
		scheme     string
		tlsRequest bool
	}{
		{"http-over-http", "http", false},
		{"http-over-https", "http", true},
		{"https-over-https", "https", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			route := Route{Host: "app.localhost", PreferredScheme: test.scheme, Target: backend.URL}
			req := httptest.NewRequest(http.MethodGet, "http://app.localhost/", nil)
			req.Host = "app.localhost"
			if test.tlsRequest {
				req.TLS = &tls.ConnectionState{}
				req.Header.Set("X-Forwarded-Proto", "http")
			}
			rec := httptest.NewRecorder()
			canonicalizationHandler(t, route).ServeHTTP(rec, req)
			if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
				t.Fatalf("status/body = %d/%q", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Strict-Transport-Security"); got != "max-age=60" {
				t.Fatalf("HSTS = %q", got)
			}
		})
	}
}

func TestRouteCanonicalizationRejectsInvalidStoredScheme(t *testing.T) {
	backendCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls++
	}))
	defer backend.Close()

	for _, scheme := range []string{"", "HTTPS", " https", "https ", "ftp"} {
		for _, tlsRequest := range []bool{false, true} {
			t.Run(fmt.Sprintf("%q/tls=%v", scheme, tlsRequest), func(t *testing.T) {
				route := Route{Host: "app.localhost", PreferredScheme: scheme, Target: backend.URL}
				req := httptest.NewRequest(http.MethodGet, "http://app.localhost/", nil)
				req.Host = "app.localhost"
				req.Header.Set("Accept", "application/json")
				if tlsRequest {
					req.TLS = &tls.ConnectionState{}
				}
				rec := httptest.NewRecorder()
				canonicalizationHandler(t, route).ServeHTTP(rec, req)
				if rec.Code != http.StatusBadGateway {
					t.Fatalf("status/body = %d/%q", rec.Code, rec.Body.String())
				}
				var payload proxyErrorPayload
				if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				if payload.Error != "invalid_route_scheme" {
					t.Fatalf("payload = %#v", payload)
				}
			})
		}
	}
	if backendCalls != 0 {
		t.Fatalf("backend calls = %d", backendCalls)
	}
}

func TestRouteCanonicalizationRejectsNonNormalizedStoredHost(t *testing.T) {
	route := Route{Host: "App.localhost", PreferredScheme: "https", Target: "http://127.0.0.1:41001"}
	for _, tlsRequest := range []bool{false, true} {
		t.Run(fmt.Sprintf("tls=%v", tlsRequest), func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://app.localhost/", nil)
			req.Host = "app.localhost"
			req.Header.Set("Accept", "application/json")
			if tlsRequest {
				req.TLS = &tls.ConnectionState{}
			}
			rec := httptest.NewRecorder()
			canonicalizationHandler(t, route).ServeHTTP(rec, req)
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status/body = %d/%q", rec.Code, rec.Body.String())
			}
			var payload proxyErrorPayload
			if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload.Error != "invalid_route_host" {
				t.Fatalf("payload = %#v", payload)
			}
		})
	}
}

func TestRouteCanonicalizationRejectsMalformedStoredHost(t *testing.T) {
	route := Route{Host: "app.localhost:bad", PreferredScheme: "https", Target: "http://127.0.0.1:41001"}
	for _, tlsRequest := range []bool{false, true} {
		t.Run(fmt.Sprintf("tls=%v", tlsRequest), func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://app.localhost/", nil)
			req.Header.Set("Accept", "application/json")
			if tlsRequest {
				req.TLS = &tls.ConnectionState{}
			}
			rec := httptest.NewRecorder()
			if !handleRouteCanonicalization(rec, req, route) {
				t.Fatal("malformed stored host was not handled")
			}
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status/body = %d/%q", rec.Code, rec.Body.String())
			}
			var payload proxyErrorPayload
			if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload.Error != "invalid_route_host" {
				t.Fatalf("payload = %#v", payload)
			}
		})
	}
}
