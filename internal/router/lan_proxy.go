package router

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

type lanProxyContextKey struct{}

type lanProxyInfo struct {
	LANHost   string
	RouteHost string
}

func withLANProxyContext(ctx context.Context, lanHost, routeHost string) context.Context {
	return context.WithValue(ctx, lanProxyContextKey{}, lanProxyInfo{
		LANHost: strings.TrimSuffix(lanHost, "."), RouteHost: strings.TrimSuffix(routeHost, "."),
	})
}

func lanProxyInfoFromContext(ctx context.Context) (lanProxyInfo, bool) {
	info, ok := ctx.Value(lanProxyContextKey{}).(lanProxyInfo)
	return info, ok && info.LANHost != "" && info.RouteHost != ""
}

func rewriteLANRequest(out, in *http.Request) {
	info, ok := lanProxyInfoFromContext(in.Context())
	if !ok {
		return
	}
	if out.Header.Get("Origin") == "https://"+info.LANHost {
		out.Header.Set("Origin", "https://"+info.RouteHost)
	}
}

func rewriteLANResponse(response *http.Response) error {
	info, ok := lanProxyInfoFromContext(response.Request.Context())
	if !ok {
		return nil
	}
	lanOrigin := "https://" + info.LANHost
	routeOrigin := "https://" + info.RouteHost
	if response.Header.Get("Access-Control-Allow-Origin") == routeOrigin {
		response.Header.Set("Access-Control-Allow-Origin", lanOrigin)
	}
	if location := response.Header.Get("Location"); location != "" {
		if parsed, err := url.Parse(location); err == nil && parsed.Scheme == "https" && strings.EqualFold(parsed.Host, info.RouteHost) {
			parsed.Host = info.LANHost
			response.Header.Set("Location", parsed.String())
		}
	}
	cookies := response.Header.Values("Set-Cookie")
	if len(cookies) > 0 {
		response.Header.Del("Set-Cookie")
		for _, cookie := range cookies {
			response.Header.Add("Set-Cookie", rewriteLANCookieDomain(cookie, info.RouteHost, info.LANHost))
		}
	}
	return nil
}

func rewriteLANCookieDomain(cookie, routeHost, lanHost string) string {
	parts := strings.Split(cookie, ";")
	for index := 1; index < len(parts); index++ {
		attribute := strings.TrimSpace(parts[index])
		name, value, ok := strings.Cut(attribute, "=")
		if !ok || !strings.EqualFold(name, "Domain") {
			continue
		}
		leadingDot := strings.HasPrefix(value, ".")
		bare := strings.TrimPrefix(value, ".")
		if !strings.EqualFold(bare, routeHost) {
			continue
		}
		if leadingDot {
			lanHost = "." + lanHost
		}
		parts[index] = " Domain=" + lanHost
	}
	return strings.Join(parts, ";")
}
