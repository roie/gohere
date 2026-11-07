package router

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	localcert "github.com/roie/gohere/internal/cert"
	"golang.org/x/net/websocket"
)

func startStreamingRouter(t *testing.T, target string) (*Running, *http.Client) {
	t.Helper()
	stateDir := t.TempDir()
	store := NewRouteStore(filepath.Join(stateDir, RoutesFilename))
	if err := store.Save([]Route{{Host: "burst.localhost", PreferredScheme: "https", Target: target}}); err != nil {
		t.Fatal(err)
	}
	certificates := &localcert.Store{StateDir: stateDir}
	ca, err := certificates.EnsureCA()
	if err != nil {
		t.Fatal(err)
	}
	running, err := Start(t.Context(), StartConfig{
		HTTPAddr: "127.0.0.1:0", HTTPSAddr: "127.0.0.1:0", AdminAddr: "127.0.0.1:0",
		StateDir: stateDir, TLSConfig: &tls.Config{}, CertificateStore: certificates,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { running.Close() })
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "burst.localhost"},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, running.HTTPSAddr)
		},
	}
	t.Cleanup(transport.CloseIdleConnections)
	return running, &http.Client{Transport: transport, Timeout: 15 * time.Second}
}

func TestHTTPSStreamingAndCancellation(t *testing.T) {
	prefixReceived := make(chan struct{})
	canceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/events":
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: ready\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			close(canceled)
		case "/upload":
			prefix := make([]byte, 5)
			if _, err := io.ReadFull(r.Body, prefix); err != nil || string(prefix) != "first" {
				t.Errorf("upload prefix = %q, %v", prefix, err)
				return
			}
			close(prefixReceived)
			io.Copy(w, r.Body)
		}
	}))
	defer upstream.Close()
	_, client := startStreamingRouter(t, upstream.URL)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "GET", "https://burst.localhost/events", nil)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	line, err := bufio.NewReader(response.Body).ReadString('\n')
	if err != nil || line != "data: ready\n" || response.ProtoMajor != 1 {
		t.Fatalf("SSE first event = %q, %v, protocol %s", line, err, response.Proto)
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("SSE cancellation did not reach upstream")
	}

	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	done := make(chan error, 1)
	go func() {
		response, err := client.Post("https://burst.localhost/upload", "application/octet-stream", reader)
		if err == nil {
			defer response.Body.Close()
			var body []byte
			body, err = io.ReadAll(response.Body)
			if err == nil && (response.StatusCode != 200 || string(body) != "last") {
				err = fmt.Errorf("upload response = %s %q", response.Status, body)
			}
		}
		done <- err
	}()
	if _, err := io.WriteString(writer, "first"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-prefixReceived:
	case <-time.After(5 * time.Second):
		t.Fatal("request body was buffered rather than streamed")
	}
	io.WriteString(writer, "last")
	writer.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestHTTPSWebSocketHMR(t *testing.T) {
	upstream := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
		defer ws.Close()
		var message string
		if err := websocket.Message.Receive(ws, &message); err != nil {
			t.Error(err)
			return
		}
		websocket.Message.Send(ws, "hmr:"+message)
	}))
	defer upstream.Close()
	running, client := startStreamingRouter(t, upstream.URL)
	response, err := client.Get("https://burst.localhost/")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.ProtoMajor != 1 {
		t.Fatalf("ordinary request protocol = %s", response.Proto)
	}
	tlsConfig := client.Transport.(*http.Transport).TLSClientConfig.Clone()
	tlsConfig.NextProtos = []string{"http/1.1"}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", running.HTTPSAddr, tlsConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	config, err := websocket.NewConfig("wss://burst.localhost/hmr", "https://burst.localhost")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := websocket.NewClient(config, conn)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	if err := websocket.Message.Send(ws, "update"); err != nil {
		t.Fatal(err)
	}
	var message string
	if err := websocket.Message.Receive(ws, &message); err != nil || message != "hmr:update" {
		t.Fatalf("HMR message = %q, %v", message, err)
	}
}
