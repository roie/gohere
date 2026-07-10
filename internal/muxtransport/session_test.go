package muxtransport

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestConfigIsBoundedForGohere(t *testing.T) {
	cfg := newConfig()
	if cfg.MaxStreamWindowSize != 256*1024 {
		t.Fatalf("stream window = %d", cfg.MaxStreamWindowSize)
	}
}

func TestStalledStreamDoesNotBlockAnotherStream(t *testing.T) {
	client, server := newSessionPair(t)
	firstClient, err := client.OpenStream(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	firstServer, err := server.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	defer firstServer.Close()

	secondClient, err := client.OpenStream(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer secondClient.Close()
	secondServer, err := server.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	defer secondServer.Close()

	stalledWrite := make(chan error, 1)
	go func() {
		_, err := io.WriteString(firstClient, strings.Repeat("x", 1024*1024))
		stalledWrite <- err
	}()
	select {
	case err := <-stalledWrite:
		t.Fatalf("first stream did not stall: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	deadline := time.Now().Add(2 * time.Second)
	_ = secondClient.SetDeadline(deadline)
	_ = secondServer.SetDeadline(deadline)
	serverResult := make(chan error, 1)
	go func() {
		request := make([]byte, 4)
		if _, err := io.ReadFull(secondServer, request); err != nil {
			serverResult <- err
			return
		}
		if string(request) != "ping" {
			serverResult <- errors.New("unexpected second-stream request")
			return
		}
		_, err := io.WriteString(secondServer, "pong")
		serverResult <- err
	}()
	if _, err := io.WriteString(secondClient, "ping"); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 4)
	if _, err := io.ReadFull(secondClient, response); err != nil {
		t.Fatal(err)
	}
	if string(response) != "pong" {
		t.Fatalf("response = %q", response)
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}

	_ = firstClient.Close()
	_ = client.Close()
	_ = server.Close()
	select {
	case <-stalledWrite:
	case <-time.After(time.Second):
		t.Fatal("stalled stream did not stop when its session closed")
	}
}

func TestStreamPreservesHalfClose(t *testing.T) {
	client, server := newSessionPair(t)
	clientStream, err := client.OpenStream(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer clientStream.Close()
	serverStream, err := server.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	defer serverStream.Close()
	deadline := time.Now().Add(2 * time.Second)
	_ = clientStream.SetDeadline(deadline)
	_ = serverStream.SetDeadline(deadline)

	serverResult := make(chan error, 1)
	go func() {
		request, err := io.ReadAll(serverStream)
		if err != nil {
			serverResult <- err
			return
		}
		if _, err := io.WriteString(serverStream, "response:"+string(request)); err != nil {
			serverResult <- err
			return
		}
		serverResult <- closeWrite(serverStream)
	}()
	if _, err := io.WriteString(clientStream, "request"); err != nil {
		t.Fatal(err)
	}
	if err := closeWrite(clientStream); err != nil {
		t.Fatal(err)
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
	response, err := io.ReadAll(clientStream)
	if err != nil {
		t.Fatal(err)
	}
	if string(response) != "response:request" {
		t.Fatalf("response = %q", response)
	}
}

func TestOpenStreamRejectsCanceledContext(t *testing.T) {
	client, _ := newSessionPair(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	stream, err := client.OpenStream(ctx)
	if stream != nil {
		_ = stream.Close()
		t.Fatal("canceled stream open returned a stream")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func newSessionPair(t *testing.T) (*Session, *Session) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	client, err := NewClient(clientConn)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(serverConn)
	if err != nil {
		_ = client.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	return client, server
}

func closeWrite(connection net.Conn) error {
	closer, ok := connection.(interface{ CloseWrite() error })
	if !ok {
		return errors.New("multiplexed stream does not support half-close")
	}
	return closer.CloseWrite()
}
