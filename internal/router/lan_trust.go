package router

import (
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"html"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	lanTrustPathPrefix       = "/g/"
	legacyLANTrustPathPrefix = "/__gohere/trust/"
	lanTrustRateLimit        = 20
	lanTrustRateWindow       = time.Minute
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
	pathPrefix := lanTrustPathPrefix
	if !strings.HasPrefix(request.URL.Path, pathPrefix) {
		pathPrefix = legacyLANTrustPathPrefix
		if !strings.HasPrefix(request.URL.Path, pathPrefix) {
			return false
		}
	}
	if request.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return true
	}
	host := request.Host
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	}
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, pathPrefix), "/")
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
	appURL := "https://" + hostname
	appURLJSON, _ := json.Marshal(appURL)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Connect this device · gohere</title><style>
:root{color-scheme:light dark;font-family:ui-sans-serif,system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;line-height:1.5}body{margin:0;background:#f4f5f7;color:#17191c}main{box-sizing:border-box;width:min(100%% - 32px,680px);margin:0 auto;padding:48px 0 64px}h1{margin:0 0 8px;font-size:2rem;line-height:1.15;letter-spacing:-.025em}h2{margin:32px 0 8px;font-size:1.15rem}p{margin:8px 0;color:#4b5159}.notice{margin:24px 0;padding:16px;background:#fff;border-radius:12px}.fingerprint{overflow-wrap:anywhere;color:#262a30}.actions{display:flex;flex-wrap:wrap;gap:10px;margin:20px 0}a.button{display:inline-block;padding:11px 16px;border-radius:8px;background:#1769e0;color:#fff;font-weight:650;text-decoration:none}a.secondary{background:#e3e7ed;color:#20242a}ol{margin:10px 0;padding-left:24px}li+li{margin-top:8px}.security{font-size:.9rem}@media(prefers-color-scheme:dark){body{background:#111315;color:#f4f5f7}.notice{background:#1d2024}p,.fingerprint{color:#c6cbd2}a.secondary{background:#30353c;color:#f4f5f7}}@media(max-width:480px){main{padding-top:28px}h1{font-size:1.65rem}.actions{display:grid}a.button{text-align:center}}
</style></head><body><main><h1>Connect this device to %s</h1><p>One-time setup. If this device already trusts gohere, you will be forwarded automatically.</p><div class="notice"><strong>Verify before installing</strong><p class="fingerprint"><code>%s</code></p><p class="security">Only install this certificate on devices you control.</p></div><section><h2>iPhone or iPad</h2><ol><li>Open this page in <strong>Safari</strong>. Chrome’s secure-connections setting may block the initial HTTP setup page.</li><li><a href="%sgohere.mobileconfig">Download the Apple profile</a>.</li><li>Open Settings → General → <strong>VPN &amp; Device Management</strong>, select the gohere profile, and tap Install.</li><li>Then open Settings → General → About → <strong>Certificate Trust Settings</strong> and enable full trust for “gohere local development CA”.</li></ol></section><section><h2>Android</h2><ol><li><a href="%sgohere-root.pem">Download the root certificate</a>.</li><li>Open Settings and search for <strong>Install a certificate</strong>.</li><li>Choose CA certificate, select the downloaded file, and confirm.</li></ol><p>Settings names vary between Android manufacturers.</p></section><div class="actions"><a class="button" id="open-app" href="%s">Open %s</a><a class="button secondary" href="%sgohere-root.pem">Download certificate</a></div></main><script>
const appURL = %s;
async function openWhenTrusted() {
  try {
    await fetch(appURL, {mode: "no-cors", cache: "no-store"});
    window.location.replace(appURL);
  } catch (_) {}
}
window.addEventListener("pageshow", openWhenTrusted);
document.addEventListener("visibilitychange", () => { if (!document.hidden) openWhenTrusted(); });
</script></body></html>`, html.EscapeString(hostname), html.EscapeString(session.Fingerprint), html.EscapeString(base), html.EscapeString(base), html.EscapeString(appURL), html.EscapeString(hostname), html.EscapeString(base), appURLJSON)
}

func lanTrustMobileConfig(session LANTrustSession) []byte {
	certificate := session.CACertificatePEM
	if block, _ := pem.Decode(certificate); block != nil {
		certificate = block.Bytes
	}
	payload := base64.StdEncoding.EncodeToString(certificate)
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>PayloadContent</key><array><dict><key>PayloadType</key><string>com.apple.security.root</string><key>PayloadIdentifier</key><string>dev.gohere.root</string><key>PayloadUUID</key><string>8E64EE1F-26F1-4B5F-9421-71AD25F93454</string><key>PayloadVersion</key><integer>1</integer><key>PayloadContent</key><data>%s</data></dict></array><key>PayloadType</key><string>Configuration</string><key>PayloadIdentifier</key><string>dev.gohere.trust</string><key>PayloadUUID</key><string>6ECBD7C6-E18E-46AF-A1A8-980DB5177E82</string><key>PayloadVersion</key><integer>1</integer><key>PayloadDisplayName</key><string>gohere local development CA</string></dict></plist>`, payload))
}
