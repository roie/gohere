package wsledge

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/roie/gohere/internal/tunnel"
)

func TestWindowsInteropSessionManagerReconnectsHelper(t *testing.T) {
	binary := os.Getenv("GOHERE_TUNNEL_HELPER")
	host := os.Getenv("GOHERE_TUNNEL_SMOKE_HOST")
	expected := os.Getenv("GOHERE_TUNNEL_SMOKE_EXPECT")
	if binary == "" || host == "" || expected == "" {
		t.Skip("set GOHERE_TUNNEL_HELPER, GOHERE_TUNNEL_SMOKE_HOST, and GOHERE_TUNNEL_SMOKE_EXPECT for the host smoke")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	starts := 0
	manager := &sessionManager{start: func(ctx context.Context) (StreamSession, error) {
		starts++
		return startHelperSession(ctx, binary, io.Discard)
	}}
	defer manager.Close()

	first, err := manager.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertInteropSessionHTTP(t, ctx, first, host, expected)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-first.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("first Windows helper session did not close")
	}

	second, err := manager.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second == first || starts != 2 {
		t.Fatalf("second session = first: %v, starts = %d", second == first, starts)
	}
	assertInteropSessionHTTP(t, ctx, second, host, expected)
}

func assertInteropSessionHTTP(t *testing.T, ctx context.Context, session StreamSession, host, expected string) {
	t.Helper()
	stream, err := session.Open(ctx, tunnel.TargetHTTP)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	_ = stream.SetDeadline(time.Now().Add(15 * time.Second))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+host+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Close = true
	if err := request.Write(stream); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(stream), request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), expected) {
		t.Fatalf("response = %s %q", response.Status, body)
	}
}
