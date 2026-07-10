package app

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"
)

func TestWindowsTunnelHandshakeStopsWhenContextExpires(t *testing.T) {
	input, inputWriter := io.Pipe()
	defer inputWriter.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	started := time.Now()
	err := ServeWindowsTunnel(ctx, input, &bytes.Buffer{}, WindowsTunnelConfig{GOOS: "windows"})
	if err == nil {
		t.Fatal("expected handshake timeout")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("handshake took %s", elapsed)
	}
}

func TestWindowsTunnelRejectsNonWindowsHost(t *testing.T) {
	err := ServeWindowsTunnel(t.Context(), bytes.NewReader(nil), &bytes.Buffer{}, WindowsTunnelConfig{GOOS: "linux"})
	if err == nil {
		t.Fatal("expected Windows requirement")
	}
}
