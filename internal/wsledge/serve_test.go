package wsledge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/roie/gohere/internal/tunnel"
)

func TestServeForwardsIPv4AndIPv6ListenerClassesThroughOneSession(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	listenerReady := make(chan struct{})
	var listenerOnce sync.Once
	var listenerMu sync.Mutex
	actualAddresses := map[string]string{}
	listen := func(_ string, requested string) (net.Listener, error) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		listenerMu.Lock()
		actualAddresses[requested] = listener.Addr().String()
		if len(actualAddresses) == 4 {
			listenerOnce.Do(func() { close(listenerReady) })
		}
		listenerMu.Unlock()
		return listener, nil
	}
	session := newEchoSession()
	startCalls := 0
	done := make(chan error, 1)
	stateDir := t.TempDir()
	go func() {
		done <- Serve(ctx, Config{
			StateDir: stateDir,
			HTTPS:    true,
			Listen:   listen,
			StartSession: func(context.Context) (StreamSession, error) {
				startCalls++
				return session, nil
			},
		})
	}()

	select {
	case <-listenerReady:
	case err := <-done:
		t.Fatalf("edge stopped before listeners became ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("edge listeners did not become ready")
	}
	listenerMu.Lock()
	httpAddress := actualAddresses["127.0.0.1:80"]
	httpsAddress := actualAddresses["[::1]:443"]
	listenerMu.Unlock()
	assertEdgeEcho(t, httpAddress, "http-data")
	assertEdgeEcho(t, httpsAddress, "https-data")

	if startCalls != 1 {
		t.Fatalf("session starts = %d, want 1", startCalls)
	}
	session.mu.Lock()
	targets := append([]tunnel.Target(nil), session.targets...)
	session.mu.Unlock()
	if len(targets) != 2 || targets[0] != tunnel.TargetHTTP || targets[1] != tunnel.TargetHTTPS {
		t.Fatalf("targets = %#v", targets)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("edge did not stop")
	}
	if _, err := os.Stat(filepath.Join(stateDir, "edge.lock")); !os.IsNotExist(err) {
		t.Fatalf("edge lock remains: %v", err)
	}
}

func TestSessionManagerReconnectsAfterHelperCloses(t *testing.T) {
	first := newEchoSession()
	second := newEchoSession()
	starts := 0
	manager := &sessionManager{start: func(context.Context) (StreamSession, error) {
		starts++
		if starts == 1 {
			return first, nil
		}
		return second, nil
	}}
	defer manager.Close()

	got, err := manager.Get(t.Context())
	if err != nil || got != first {
		t.Fatalf("first session = %#v, err = %v", got, err)
	}
	_ = first.Close()
	got, err = manager.Get(t.Context())
	if err != nil || got != second || starts != 2 {
		t.Fatalf("second session = %#v, starts = %d, err = %v", got, starts, err)
	}
}

func TestEdgeLockUsesProcessIdentityAndRejectsPIDReuse(t *testing.T) {
	stateDir := t.TempDir()
	lockPath := filepath.Join(stateDir, "edge.lock")
	lock, err := acquireEdgeLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if !Running(stateDir) {
		t.Fatal("current edge lock is not reported as running")
	}
	if _, err := acquireEdgeLock(lockPath); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second lock error = %v", err)
	}
	lock.Release()

	stale := edgeLockRecord{PID: os.Getpid(), ProcessIdentity: "linux:not-this-process"}
	data, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := Stop(stateDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("stale reused-PID lock remains: %v", err)
	}
}

func TestStopRefusesLiveLegacyPIDOnlyEdgeLock(t *testing.T) {
	stateDir := t.TempDir()
	lockPath := filepath.Join(stateDir, "edge.lock")
	if err := os.WriteFile(lockPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Stop(stateDir); err == nil || !strings.Contains(err.Error(), "unverifiable legacy") {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("legacy live lock was removed: %v", err)
	}
}

func assertEdgeEcho(t *testing.T, address, message string) {
	t.Helper()
	connection, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := io.WriteString(connection, message); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, len(message))
	if _, err := io.ReadFull(connection, response); err != nil {
		t.Fatal(err)
	}
	if string(response) != message {
		t.Fatalf("response = %q, want %q", response, message)
	}
}

type echoSession struct {
	mu      sync.Mutex
	targets []tunnel.Target
	done    chan struct{}
	once    sync.Once
}

func newEchoSession() *echoSession { return &echoSession{done: make(chan struct{})} }

func (s *echoSession) Open(_ context.Context, target tunnel.Target) (net.Conn, error) {
	s.mu.Lock()
	s.targets = append(s.targets, target)
	s.mu.Unlock()
	left, right := net.Pipe()
	go func() {
		defer right.Close()
		_, _ = io.Copy(right, right)
	}()
	return left, nil
}

func (s *echoSession) Close() error {
	s.once.Do(func() { close(s.done) })
	return nil
}

func (s *echoSession) Done() <-chan struct{} { return s.done }
