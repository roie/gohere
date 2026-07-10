package tunnel

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWindowsInteropHelperSmoke(t *testing.T) {
	client, ctx := startWindowsInteropClient(t)
	host := os.Getenv("GOHERE_TUNNEL_SMOKE_HOST")
	expected := os.Getenv("GOHERE_TUNNEL_SMOKE_EXPECT")
	minimumBytes := 0
	if raw := os.Getenv("GOHERE_TUNNEL_SMOKE_MIN_BYTES"); raw != "" {
		var err error
		minimumBytes, err = strconv.Atoi(raw)
		if err != nil {
			t.Fatalf("GOHERE_TUNNEL_SMOKE_MIN_BYTES: %v", err)
		}
	}
	if host == "" {
		host = "gohere-tunnel-smoke.localhost"
	}

	const requests = 12
	var wg sync.WaitGroup
	errors := make(chan error, requests)
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stream, err := client.Open(ctx, TargetHTTP)
			if err != nil {
				errors <- err
				return
			}
			defer stream.Close()
			_ = stream.SetDeadline(time.Now().Add(15 * time.Second))
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+host+"/", nil)
			if err != nil {
				errors <- err
				return
			}
			request.Close = true
			if err := request.Write(stream); err != nil {
				errors <- err
				return
			}
			response, err := http.ReadResponse(bufio.NewReader(stream), request)
			if err != nil {
				errors <- err
				return
			}
			body, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil {
				errors <- err
				return
			}
			text := string(body)
			if expected != "" && (response.StatusCode != http.StatusOK || !strings.Contains(text, expected) || len(body) < minimumBytes) {
				errors <- &unexpectedTunnelResponse{response: response.Status + " " + text}
			} else if expected == "" && (response.StatusCode != http.StatusBadGateway || !strings.Contains(text, "gohere")) {
				errors <- &unexpectedTunnelResponse{response: response.Status + " " + text}
			}
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
}

func TestWindowsInteropHelperWebSocketUpgrade(t *testing.T) {
	host := os.Getenv("GOHERE_TUNNEL_SMOKE_WEBSOCKET_HOST")
	path := os.Getenv("GOHERE_TUNNEL_SMOKE_WEBSOCKET_PATH")
	if host == "" || path == "" {
		t.Skip("set GOHERE_TUNNEL_SMOKE_WEBSOCKET_HOST and GOHERE_TUNNEL_SMOKE_WEBSOCKET_PATH from a browser-observed Vite route")
	}
	client, ctx := startWindowsInteropClient(t)
	stream, err := client.Open(ctx, TargetHTTP)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	_ = stream.SetDeadline(time.Now().Add(10 * time.Second))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+host+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Origin", "http://"+host)
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	request.Header.Set("Sec-WebSocket-Protocol", "vite-hmr")
	if err := request.Write(stream); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(stream), request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("WebSocket status = %s", response.Status)
	}
}

func startWindowsInteropClient(t *testing.T) (*Client, context.Context) {
	t.Helper()
	binary := os.Getenv("GOHERE_TUNNEL_HELPER")
	if binary == "" {
		t.Skip("set GOHERE_TUNNEL_HELPER to a Windows gohere.exe for the host smoke")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	command := exec.CommandContext(ctx, binary, InternalCommand)
	stdin, err := command.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	client, err := NewClient(ctx, &PipeConn{
		Reader: stdout,
		Writer: stdin,
		Closer: &interopProcessCloser{command: command, stdin: stdin, stdout: stdout},
	}, &stderr)
	if err != nil {
		cancel()
		t.Fatalf("start tunnel client: %v\n%s", err, stderr.String())
	}
	t.Cleanup(func() {
		cancel()
		_ = client.Close()
		_ = stdin.Close()
		_ = stdout.Close()
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		_ = command.Wait()
	})
	return client, ctx
}

type unexpectedTunnelResponse struct{ response string }

func (e *unexpectedTunnelResponse) Error() string { return "unexpected tunnel response: " + e.response }

type interopProcessCloser struct {
	command *exec.Cmd
	stdin   io.Closer
	stdout  io.Closer
	once    sync.Once
}

func (c *interopProcessCloser) Close() error {
	c.once.Do(func() {
		_ = c.stdin.Close()
		_ = c.stdout.Close()
		if c.command.Process != nil {
			_ = c.command.Process.Kill()
		}
	})
	return nil
}
