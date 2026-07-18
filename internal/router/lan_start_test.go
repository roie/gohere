package router

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"testing"
	"time"

	goherecert "github.com/roie/gohere/internal/cert"
)

func TestAdminWriteTimeoutAllowsWindowsFirewallApproval(t *testing.T) {
	if adminWriteTimeout < 2*time.Minute {
		t.Fatalf("admin write timeout = %s, want at least 2m for Windows firewall approval", adminWriteTimeout)
	}
}

func TestStartLANIngressServesSharedHTTPAndHTTPSListeners(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	upstreamServer := &http.Server{Handler: upstream}
	upstreamListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstreamListener.Close()
	go upstreamServer.Serve(upstreamListener)
	defer upstreamServer.Close()

	certificateStore := goherecert.Store{StateDir: t.TempDir()}
	certificate, err := certificateStore.IssueEphemeralLANHostCert("shop.local", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ca, err := certificateStore.EnsureCA()
	if err != nil {
		t.Fatal(err)
	}
	route := Route{ID: "route-1", Generation: 1, State: RouteStateActive, Host: "shop.localhost", Target: "http://" + upstreamListener.Addr().String(), PreferredScheme: "https",
		LANShare: &LANShare{State: LANShareActive, RequestedHostname: "shop.local.", Hostname: "shop.local.", CreatedAt: time.Now()},
	}
	store := NewMemoryStore()
	if err := store.Save([]Route{route}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(Config{Store: store})
	if err := server.RegisterLANRoute(route.Ref(), "shop.local", certificate); err != nil {
		t.Fatal(err)
	}
	ingress, err := StartLANIngress(t.Context(), server, "127.0.0.1:0", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ingress.Close()

	redirectClient := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequest(http.MethodGet, "http://"+ingress.HTTPAddr()+"/asset?q=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "shop.local"
	response, err := redirectClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusPermanentRedirect || response.Header.Get("Location") != "https://shop.local/asset?q=1" {
		t.Fatalf("redirect = %d %q", response.StatusCode, response.Header.Get("Location"))
	}

	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: "shop.local"},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, ingress.HTTPSAddr())
		},
	}
	httpsClient := &http.Client{Transport: transport}
	httpsResponse, err := httpsClient.Get("https://shop.local/")
	if err != nil {
		t.Fatal(err)
	}
	httpsResponse.Body.Close()
	if httpsResponse.StatusCode != http.StatusNoContent {
		t.Fatalf("HTTPS status = %d", httpsResponse.StatusCode)
	}
}

func TestLANIngressCloseIsIdempotent(t *testing.T) {
	ingress, err := StartLANIngress(t.Context(), NewServer(Config{Store: NewMemoryStore()}), "127.0.0.1:0", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := ingress.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ingress.Close(); err != nil {
		t.Fatal(err)
	}
}
