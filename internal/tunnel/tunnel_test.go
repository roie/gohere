package tunnel

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTunnelMultiplexesConcurrentStreams(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- Serve(ctx, serverConn, ServerConfig{
			Dial: func(context.Context, string, string) (net.Conn, error) {
				left, right := net.Pipe()
				go func() {
					defer right.Close()
					_, _ = io.Copy(right, right)
				}()
				return left, nil
			},
		})
	}()
	client, err := NewClient(ctx, clientConn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	const streams = 32
	var wg sync.WaitGroup
	errors := make(chan error, streams)
	for i := 0; i < streams; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stream, err := client.Open(ctx, TargetHTTP)
			if err != nil {
				errors <- err
				return
			}
			defer stream.Close()
			message := strings.Repeat("edge-data-", 1024)
			if _, err := io.WriteString(stream, message); err != nil {
				errors <- err
				return
			}
			response := make([]byte, len(message))
			if _, err := io.ReadFull(stream, response); err != nil {
				errors <- err
				return
			}
			if string(response) != message {
				errors <- io.ErrUnexpectedEOF
			}
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("tunnel server did not stop")
	}
}

func TestTunnelRejectsHTTPSWhenNotDeclared(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go Serve(ctx, serverConn, ServerConfig{})
	client, err := NewClient(ctx, clientConn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	_, err = client.Open(ctx, TargetHTTPS)
	if err == nil || !strings.Contains(err.Error(), "rejected target") {
		t.Fatalf("error = %v", err)
	}
}

func TestTunnelRejectsInvalidHandshake(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	done := make(chan error, 1)
	go func() { done <- Serve(t.Context(), server, ServerConfig{}) }()
	if _, err := client.Write([]byte(strings.Repeat("x", len(protocolPreface)))); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "invalid WSL tunnel handshake") {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("invalid handshake was not rejected")
	}
}

func TestTunnelPreservesTCPHalfClose(t *testing.T) {
	upstreamListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstreamListener.Close()

	upstreamDone := make(chan error, 1)
	go func() {
		connection, err := upstreamListener.Accept()
		if err != nil {
			upstreamDone <- err
			return
		}
		defer connection.Close()
		request, err := io.ReadAll(connection)
		if err != nil {
			upstreamDone <- err
			return
		}
		_, err = io.WriteString(connection, "response:"+string(request))
		upstreamDone <- err
	}()

	clientConn, serverConn := net.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- Serve(ctx, serverConn, ServerConfig{
			Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", upstreamListener.Addr().String())
			},
		})
	}()

	client, err := NewClient(ctx, clientConn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	stream, err := client.Open(ctx, TargetHTTP)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if err := stream.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(stream, "request"); err != nil {
		t.Fatal(err)
	}
	closeWriter, ok := stream.(interface{ CloseWrite() error })
	if !ok {
		t.Fatal("multiplexed stream does not support TCP half-close")
	}
	if err := closeWriter.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	response, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	if string(response) != "response:request" {
		t.Fatalf("response = %q", response)
	}
	if err := <-upstreamDone; err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("tunnel server did not stop")
	}
}

func TestClientHandshakeStopsWhenContextExpires(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, err := NewClient(ctx, client, nil)
	if err == nil {
		t.Fatal("expected handshake timeout")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("handshake took %s", elapsed)
	}
}

func TestClientOpenStopsWhenContextExpires(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	go func() {
		request := make([]byte, len(protocolPreface))
		if _, err := io.ReadFull(serverConn, request); err != nil {
			return
		}
		_, _ = serverConn.Write(protocolPreface)
	}()
	client, err := NewClient(t.Context(), clientConn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = client.Open(ctx, TargetHTTP)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("stream open took %s", elapsed)
	}
}

func TestServerHandshakeStopsWhenContextExpires(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	started := time.Now()
	err := Serve(ctx, server, ServerConfig{})
	if err == nil {
		t.Fatal("expected handshake timeout")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("handshake took %s", elapsed)
	}
}
