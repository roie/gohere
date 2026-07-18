package router

import (
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"html"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	lanTrustPathPrefix = "/__gohere/trust/"
	lanTrustRateLimit  = 20
	lanTrustRateWindow = time.Minute
)

type LANTrustSession struct {
	Address          string
	Token            string
	Hostname         string
	Fingerprint      string
	CACertificatePEM []byte
}

type lanTrustAttempt struct {
	window time.Time
	count  int
}

func (s *Server) ConfigureLANTrust(session LANTrustSession) {
	copy := session
	copy.CACertificatePEM = append([]byte(nil), session.CACertificatePEM...)
	s.lanTrustMu.Lock()
	if s.lanTrustSessions == nil {
		s.lanTrustSessions = make(map[string]LANTrustSession)
	}
	s.lanTrustSessions[session.Token] = copy
	s.lanTrustMu.Unlock()
}

func (s *Server) ClearLANTrust(tokens ...string) {
	s.lanTrustMu.Lock()
	if len(tokens) == 0 {
		s.lanTrustSessions = make(map[string]LANTrustSession)
		s.lanTrustAttempts = make(map[string]lanTrustAttempt)
	} else {
		for _, token := range tokens {
			delete(s.lanTrustSessions, token)
		}
	}
	s.lanTrustMu.Unlock()
}

func (s *Server) serveLANTrust(w http.ResponseWriter, request *http.Request) bool {
	if !strings.HasPrefix(request.URL.Path, lanTrustPathPrefix) {
		return false
	}
	if request.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return true
	}
	host := request.Host
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	}
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, lanTrustPathPrefix), "/")
	client, _, _ := net.SplitHostPort(request.RemoteAddr)
	if client == "" {
		client = request.RemoteAddr
	}
	s.lanTrustMu.Lock()
	defer s.lanTrustMu.Unlock()
	var session LANTrustSession
	valid := false
	if len(parts) > 0 {
		session, valid = s.lanTrustSessions[parts[0]]
		valid = valid && host == session.Address
	}
	if !valid {
		if s.rateLimitedLANTrust(client, time.Now()) {
			http.Error(w, "too many setup attempts", http.StatusTooManyRequests)
		} else {
			http.NotFound(w, request)
		}
		return true
	}
	if len(parts) == 1 && parts[0] != "" {
		s.writeLANTrustPage(w, session)
		return true
	}
	if len(parts) != 2 {
		http.NotFound(w, request)
		return true
	}
	switch parts[1] {
	case "gohere-root.pem":
		w.Header().Set("Content-Type", "application/x-pem-file")
		w.Header().Set("Content-Disposition", `attachment; filename="gohere-root.pem"`)
		_, _ = w.Write(session.CACertificatePEM)
	case "gohere.mobileconfig":
		w.Header().Set("Content-Type", "application/x-apple-aspen-config")
		w.Header().Set("Content-Disposition", `attachment; filename="gohere.mobileconfig"`)
		_, _ = w.Write(lanTrustMobileConfig(session))
	default:
		http.NotFound(w, request)
	}
	return true
}

func (s *Server) rateLimitedLANTrust(client string, now time.Time) bool {
	attempt := s.lanTrustAttempts[client]
	if attempt.window.IsZero() || now.Sub(attempt.window) >= lanTrustRateWindow {
		attempt = lanTrustAttempt{window: now}
	}
	if attempt.count >= lanTrustRateLimit {
		return true
	}
	attempt.count++
	s.lanTrustAttempts[client] = attempt
	return false
}

func (s *Server) writeLANTrustPage(w http.ResponseWriter, session LANTrustSession) {
	base := lanTrustPathPrefix + session.Token + "/"
	hostname := strings.TrimSuffix(session.Hostname, ".")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Trust gohere</title></head><body><main><h1>Trust gohere on this device</h1><p>Only install this certificate on devices you control. Verify the fingerprint before enabling trust.</p><p><strong>CA fingerprint:</strong> <code>%s</code></p><p><a href="%sgohere-root.pem">Download gohere-root.pem</a></p><p><a href="%sgohere.mobileconfig">Download gohere.mobileconfig for Apple devices</a></p><h2>Apple devices</h2><p>Install the profile, then enable the root under Settings → General → About → Certificate Trust Settings.</p><h2>Android, Windows, macOS, and Linux</h2><p>Install the downloaded public root certificate in the system trust store and compare its fingerprint above.</p><p>After trust is enabled, open <a href="https://%s">https://%s</a>.</p></main></body></html>`, html.EscapeString(session.Fingerprint), html.EscapeString(base), html.EscapeString(base), html.EscapeString(hostname), html.EscapeString(hostname))
}

func lanTrustMobileConfig(session LANTrustSession) []byte {
	certificate := session.CACertificatePEM
	if block, _ := pem.Decode(certificate); block != nil {
		certificate = block.Bytes
	}
	payload := base64.StdEncoding.EncodeToString(certificate)
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>PayloadContent</key><array><dict><key>PayloadType</key><string>com.apple.security.root</string><key>PayloadIdentifier</key><string>dev.gohere.root</string><key>PayloadUUID</key><string>8E64EE1F-26F1-4B5F-9421-71AD25F93454</string><key>PayloadVersion</key><integer>1</integer><key>PayloadContent</key><data>%s</data></dict></array><key>PayloadType</key><string>Configuration</string><key>PayloadIdentifier</key><string>dev.gohere.trust</string><key>PayloadUUID</key><string>6ECBD7C6-E18E-46AF-A1A8-980DB5177E82</string><key>PayloadVersion</key><integer>1</integer><key>PayloadDisplayName</key><string>gohere local development CA</string></dict></plist>`, payload))
}
