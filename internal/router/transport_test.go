package router

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestProxyConnectionBursts(t *testing.T) {
	for _, busy := range []bool{false, true} {
		for _, n := range []int{300, 470, 1000} {
			t.Run(fmt.Sprintf("busy=%v/n=%d", busy, n), func(t *testing.T) {
				var connections, peak atomic.Int64
				upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					time.Sleep(20 * time.Millisecond)
					io.WriteString(w, "module")
				}))
				upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
					if state == http.StateNew {
						current := connections.Add(1)
						for previous := peak.Load(); current > previous && !peak.CompareAndSwap(previous, current); previous = peak.Load() {
						}
					}
					if state == http.StateClosed {
						connections.Add(-1)
					}
				}
				if busy {
					upstream.Listener = &pausedAcceptListener{Listener: upstream.Listener, until: time.Now().Add(500 * time.Millisecond)}
				}
				upstream.Start()
				defer upstream.Close()
				server := NewServer(Config{Store: NewMemoryStore()})
				if err := server.store.Save([]Route{{Host: "burst.localhost", PreferredScheme: "http", Target: upstream.URL}}); err != nil {
					t.Fatal(err)
				}
				defer server.proxyTransport.CloseIdleConnections()
				handler := server.HTTPHandler()
				var wg sync.WaitGroup
				start := make(chan struct{})
				for range n {
					wg.Go(func() {
						<-start
						ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
						defer cancel()
						request := httptest.NewRequestWithContext(ctx, "GET", "http://burst.localhost/module.js", nil)
						response := httptest.NewRecorder()
						handler.ServeHTTP(response, request)
						if response.Code != http.StatusOK || response.Body.String() != "module" {
							t.Errorf("response = %d %q", response.Code, response.Body.String())
						}
					})
				}
				close(start)
				wg.Wait()
				if got := peak.Load(); got > 64 {
					t.Errorf("peak upstream connections = %d, want <= 64", got)
				}
			})
		}
	}
}

type pausedAcceptListener struct {
	net.Listener
	until time.Time
}

func (l *pausedAcceptListener) Accept() (net.Conn, error) {
	if delay := time.Until(l.until); delay > 0 {
		time.Sleep(delay)
	}
	return l.Listener.Accept()
}

func testProxyServer(t *testing.T, target string) (*Server, *httptest.Server) {
	t.Helper()
	store := NewMemoryStore()
	if err := store.Save([]Route{{Host: "burst.localhost", PreferredScheme: "http", Target: target}}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(Config{Store: store})
	proxy := httptest.NewServer(server.HTTPHandler())
	t.Cleanup(proxy.Close)
	return server, proxy
}

func TestProxyAdmissionBound(t *testing.T) {
	server, proxy := testProxyServer(t, "http://127.0.0.1:1")
	for range maxProxyRequests {
		server.proxyRequests <- struct{}{}
	}
	request, _ := http.NewRequestWithContext(t.Context(), "GET", proxy.URL, nil)
	request.Host = "burst.localhost"
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.StatusCode)
	}
	if len(server.proxyRequests) != maxProxyRequests {
		t.Fatal("rejected request changed admission count")
	}
}

func TestProxyQueuedCancellation(t *testing.T) {
	entered := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { entered <- struct{}{}; <-r.Context().Done() }))
	defer upstream.Close()
	server, proxy := testProxyServer(t, upstream.URL)
	server.proxyTransport.MaxConnsPerHost = 1
	defer server.proxyTransport.CloseIdleConnections()
	firstCtx, cancelFirst := context.WithCancel(t.Context())
	defer cancelFirst()
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		request, _ := http.NewRequestWithContext(firstCtx, "GET", proxy.URL, nil)
		request.Host = "burst.localhost"
		response, _ := http.DefaultClient.Do(request)
		if response != nil {
			response.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first request did not enter upstream")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "GET", proxy.URL, nil)
	request.Host = "burst.localhost"
	if response, err := http.DefaultClient.Do(request); err == nil {
		response.Body.Close()
		t.Fatal("queued request did not cancel")
	}
	cancelFirst()
	<-firstDone
	select {
	case <-entered:
		t.Fatal("canceled queued request reached upstream")
	default:
	}
}
