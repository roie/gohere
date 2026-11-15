package router

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProxyRespondsToPausedUpload(t *testing.T) {
	for _, secure := range []bool{false, true} {
		for _, status := range []int{http.StatusRequestEntityTooLarge, http.StatusBadGateway} {
			for _, size := range []int{-1, 64 << 10, 1 << 20} {
				t.Run(fmt.Sprintf("tls=%v/status=%d/size=%d", secure, status, size), func(t *testing.T) {
					target := failingUpstreamTarget(t)
					if status == http.StatusRequestEntityTooLarge {
						upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							if r.Method == http.MethodGet {
								io.WriteString(w, "ok")
								return
							}
							w.Header().Set("Connection", "close")
							w.Header().Set("Content-Length", "9")
							w.WriteHeader(http.StatusRequestEntityTooLarge)
							io.WriteString(w, "too large")
							if err := http.NewResponseController(w).Flush(); err != nil {
								return
							}
							io.Copy(io.Discard, r.Body)
						}))
						t.Cleanup(upstream.Close)
						target = upstream.URL
					}
					front := uploadProxy(t, target, secure)
					conn := uploadConnection(t, front, secure)
					defer conn.Close()
					reader := bufio.NewReader(conn)
					if status == http.StatusRequestEntityTooLarge {
						if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: web.localhost\r\n\r\n"); err != nil {
							t.Fatal(err)
						}
						warmup, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
						if err != nil {
							t.Fatal(err)
						}
						io.Copy(io.Discard, warmup.Body)
						warmup.Body.Close()
						if warmup.StatusCode != http.StatusOK || warmup.Close {
							t.Fatal("warm-up request did not leave a reusable connection")
						}
					}
					headers := fmt.Sprintf("Content-Length: %d\r\n\r\n", size)
					prefix := strings.Repeat("x", 1024)
					if size < 0 {
						headers = "Transfer-Encoding: chunked\r\n\r\n"
						prefix = "400\r\n" + prefix + "\r\n"
					}
					if _, err := fmt.Fprintf(conn, "POST /upload HTTP/1.1\r\nHost: web.localhost\r\n%s%s", headers, prefix); err != nil {
						t.Fatal(err)
					}
					response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodPost})
					if err != nil {
						t.Fatalf("response stalled while upload was paused: %v", err)
					}
					body, err := io.ReadAll(response.Body)
					response.Body.Close()
					if err != nil {
						t.Fatal(err)
					}
					if response.StatusCode != status {
						t.Fatalf("status = %d, want %d; body = %q", response.StatusCode, status, body)
					}
					if status == http.StatusRequestEntityTooLarge && string(body) != "too large" {
						t.Fatalf("body = %q", body)
					}
					if !response.Close {
						t.Fatal("unfinished upload connection was advertised as reusable")
					}
					if _, err := conn.Read(make([]byte, 1)); err != io.EOF {
						t.Fatalf("connection did not close after response: %v", err)
					}

					followup := uploadConnection(t, front, secure)
					defer followup.Close()
					if _, err := io.WriteString(followup, "GET / HTTP/1.1\r\nHost: web.localhost\r\nConnection: close\r\n\r\n"); err != nil {
						t.Fatal(err)
					}
					next, err := http.ReadResponse(bufio.NewReader(followup), &http.Request{Method: http.MethodGet})
					if err != nil {
						t.Fatal(err)
					}
					defer next.Body.Close()
					want := http.StatusOK
					if status == http.StatusBadGateway {
						want = status
					}
					if next.StatusCode != want {
						t.Fatalf("follow-up status = %d, want %d", next.StatusCode, want)
					}
				})
			}
		}
	}
}

func TestProxyCompletedUploadKeepsConnectionAlive(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprintf("tls=%v", secure), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != "complete" {
					t.Errorf("upload = %q, %v", body, err)
					return
				}
				io.WriteString(w, "ok")
			}))
			defer upstream.Close()
			front := uploadProxy(t, upstream.URL, secure)
			conn := uploadConnection(t, front, secure)
			defer conn.Close()
			reader := bufio.NewReader(conn)
			for range 2 {
				if _, err := io.WriteString(conn, "POST / HTTP/1.1\r\nHost: web.localhost\r\nContent-Length: 8\r\n\r\ncomplete"); err != nil {
					t.Fatal(err)
				}
				response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodPost})
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil || response.StatusCode != http.StatusOK || string(body) != "ok" || response.Close {
					t.Fatalf("completed upload response = %d %q, close=%v, error=%v", response.StatusCode, body, response.Close, err)
				}
			}
		})
	}
}

func uploadProxy(t *testing.T, target string, secure bool) *httptest.Server {
	t.Helper()
	store := NewMemoryStore()
	if err := store.Save([]Route{{Host: "web.localhost", PreferredScheme: "http", Target: target}}); err != nil {
		t.Fatal(err)
	}
	router := NewServer(Config{Store: store})
	front := httptest.NewUnstartedServer(router.HTTPHandler())
	if secure {
		front.StartTLS()
	} else {
		front.Start()
	}
	t.Cleanup(func() { front.Close(); router.proxyTransport.CloseIdleConnections() })
	return front
}

func uploadConnection(t *testing.T, front *httptest.Server, secure bool) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", front.Listener.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	if secure {
		client := tls.Client(conn, &tls.Config{RootCAs: front.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs, ServerName: "127.0.0.1"})
		conn = client
	}
	return conn
}
