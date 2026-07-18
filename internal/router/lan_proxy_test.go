package router

import (
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLANProxyRewritesExactHeadersWithoutRewritingBody(t *testing.T) {
	observed := make(chan *http.Request, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		observed <- request.Clone(request.Context())
		w.Header().Set("Access-Control-Allow-Origin", "https://shop.localhost")
		w.Header().Set("Location", "https://shop.localhost/login?q=1")
		w.Header().Add("Set-Cookie", "sid=one; Domain=shop.localhost; Path=/; HttpOnly")
		w.Header().Add("Set-Cookie", "hostonly=two; Path=/")
		_, _ = io.WriteString(w, `{"url":"https://shop.localhost/inside-body"}`)
	}))
	defer upstream.Close()

	route := Route{ID: "route-1", Generation: 1, State: RouteStateActive, Host: "shop.localhost", Target: upstream.URL, PreferredScheme: "https",
		LANShare: &LANShare{State: LANShareActive, RequestedHostname: "shop.local.", Hostname: "shop.local.", CreatedAt: time.Now()},
	}
	store := NewMemoryStore()
	if err := store.Save([]Route{route}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(Config{Store: store})
	if err := server.RegisterLANRoute(route.Ref(), "shop.local", tls.Certificate{}); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "https://shop.local/api", strings.NewReader("request body"))
	request.Host = "shop.local"
	request.RemoteAddr = "192.168.1.58:43210"
	request.TLS = &tls.ConnectionState{ServerName: "shop.local"}
	request.Header.Set("Origin", "https://shop.local")
	request.Header.Set("Forwarded", "for=attacker")
	request.Header.Set("X-Forwarded-For", "203.0.113.9")
	request.Header.Set("X-Forwarded-Host", "private.localhost")
	request.Header.Set("X-Forwarded-Proto", "http")
	request.Header.Set("X-Forwarded-Port", "1234")
	response := httptest.NewRecorder()
	server.LANHandler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}

	upstreamRequest := <-observed
	if upstreamRequest.Host != "shop.localhost" || upstreamRequest.Header.Get("Origin") != "https://shop.localhost" {
		t.Fatalf("upstream host/origin = %q / %q", upstreamRequest.Host, upstreamRequest.Header.Get("Origin"))
	}
	if upstreamRequest.Header.Get("X-Forwarded-Host") != "shop.local" || upstreamRequest.Header.Get("X-Forwarded-Proto") != "https" {
		t.Fatalf("forwarded host/proto = %q / %q", upstreamRequest.Header.Get("X-Forwarded-Host"), upstreamRequest.Header.Get("X-Forwarded-Proto"))
	}
	if upstreamRequest.Header.Get("Forwarded") != "" || upstreamRequest.Header.Get("X-Forwarded-Port") != "" || upstreamRequest.Header.Get("X-Forwarded-For") != "192.168.1.58" {
		t.Fatalf("forwarding headers = %#v", upstreamRequest.Header)
	}
	if response.Header().Get("Access-Control-Allow-Origin") != "https://shop.local" {
		t.Fatalf("ACAO = %q", response.Header().Get("Access-Control-Allow-Origin"))
	}
	if response.Header().Get("Location") != "https://shop.local/login?q=1" {
		t.Fatalf("Location = %q", response.Header().Get("Location"))
	}
	cookies := response.Header().Values("Set-Cookie")
	if len(cookies) != 2 || !strings.Contains(cookies[0], "Domain=shop.local") || strings.Contains(cookies[1], "Domain=") {
		t.Fatalf("Set-Cookie = %#v", cookies)
	}
	if response.Body.String() != `{"url":"https://shop.localhost/inside-body"}` {
		t.Fatalf("body was rewritten: %s", response.Body.String())
	}
}

func TestLANProxyDoesNotRewriteNonmatchingOrigin(t *testing.T) {
	origin := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		origin <- request.Header.Get("Origin")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	route := Route{ID: "route-1", Generation: 1, State: RouteStateActive, Host: "shop.localhost", Target: upstream.URL, PreferredScheme: "https",
		LANShare: &LANShare{State: LANShareActive, Hostname: "shop.local.", CreatedAt: time.Now()},
	}
	store := NewMemoryStore()
	_ = store.Save([]Route{route})
	server := NewServer(Config{Store: store})
	_ = server.RegisterLANRoute(route.Ref(), "shop.local", tls.Certificate{})
	request := httptest.NewRequest(http.MethodGet, "https://shop.local/", nil)
	request.Host = "shop.local"
	request.TLS = &tls.ConnectionState{ServerName: "shop.local"}
	request.Header.Set("Origin", "https://other.local")
	server.LANHandler().ServeHTTP(httptest.NewRecorder(), request)
	if got := <-origin; got != "https://other.local" {
		t.Fatalf("Origin = %q", got)
	}
}
