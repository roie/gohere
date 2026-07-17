package router

import (
	"net/http"
	"net/url"
	"strings"
)

func validPreferredScheme(scheme string) bool {
	return scheme == "http" || scheme == "https"
}

func handleRouteCanonicalization(w http.ResponseWriter, r *http.Request, route Route) bool {
	normalizedHost, err := normalizeRouteHost(route.Host)
	if err != nil || normalizedHost != route.Host {
		writeProxyError(w, r, http.StatusBadGateway, proxyErrorPayload{
			Error:   "invalid_route_host",
			Message: "invalid gohere route host",
		})
		return true
	}

	if !validPreferredScheme(route.PreferredScheme) {
		writeProxyError(w, r, http.StatusBadGateway, proxyErrorPayload{
			Error:   "invalid_route_scheme",
			Message: "invalid gohere route scheme",
			Host:    route.Host,
		})
		return true
	}
	if route.PreferredScheme == "http" || r.TLS != nil {
		return false
	}

	switch {
	case isCORSPreflight(r):
		w.Header().Set("Cache-Control", "no-store")
		writeProxyError(w, r, http.StatusBadRequest, proxyErrorPayload{
			Error:   "secure_transport_required",
			Message: "gohere route requires secure transport; use " + secureRouteURL("https", route.Host, r.URL, false),
			Host:    route.Host,
		})
	case isWebSocketUpgradeAttempt(r):
		w.Header().Set("Cache-Control", "no-store")
		writeProxyError(w, r, http.StatusBadRequest, proxyErrorPayload{
			Error:   "secure_websocket_required",
			Message: "gohere route requires secure WebSocket; connect to " + secureRouteURL("wss", route.Host, r.URL, false),
			Host:    route.Host,
		})
	default:
		writeTemporaryHTTPSRedirect(w, secureRouteURL("https", route.Host, r.URL, true))
	}
	return true
}

func isCORSPreflight(r *http.Request) bool {
	return r.Method == http.MethodOptions &&
		strings.TrimSpace(r.Header.Get("Origin")) != "" &&
		strings.TrimSpace(r.Header.Get("Access-Control-Request-Method")) != ""
}

func isWebSocketUpgradeAttempt(r *http.Request) bool {
	return r.Method == http.MethodGet &&
		headerListContainsToken(r.Header.Values("Upgrade"), "websocket") &&
		headerListContainsToken(r.Header.Values("Connection"), "upgrade")
}

func headerListContainsToken(values []string, token string) bool {
	for _, value := range values {
		for _, item := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(item), token) {
				return true
			}
		}
	}
	return false
}

func secureRouteURL(scheme, host string, requestURL *url.URL, includeQuery bool) string {
	destination := url.URL{
		Scheme:  scheme,
		Host:    host,
		Path:    requestURL.Path,
		RawPath: requestURL.RawPath,
	}
	if includeQuery {
		destination.RawQuery = requestURL.RawQuery
		destination.ForceQuery = requestURL.ForceQuery
	}
	return destination.String()
}

func writeTemporaryHTTPSRedirect(w http.ResponseWriter, location string) {
	w.Header().Set("Location", location)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusTemporaryRedirect)
}
