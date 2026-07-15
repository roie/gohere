package wsledge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
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

func TestEdgeLockUsesProcessAndBinaryIdentityAndRejectsPIDReuse(t *testing.T) {
	stateDir := t.TempDir()
	lockPath := filepath.Join(stateDir, "edge.lock")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	edgeHash, err := fileSHA256Hex(executable)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := acquireEdgeLock(lockPath, executable, edgeHash, `C:\\Temp\\gohere.exe`)
	if err != nil {
		t.Fatal(err)
	}
	record, err := readEdgeLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if record.PID != os.Getpid() || record.EdgeBinary != executable || record.EdgeSHA256 != edgeHash || record.CompanionBinary != `C:\\Temp\\gohere.exe` {
		t.Fatalf("lock record = %#v", record)
	}
	if _, err := acquireEdgeLock(lockPath, executable, edgeHash, `C:\\Temp\\gohere.exe`); !errors.Is(err, ErrAlreadyRunning) {
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

func TestInspectRecoversVerifiedLegacyEdgeIdentity(t *testing.T) {
	if os.Getenv("GOHERE_LEGACY_EDGE_HELPER") == "1" {
		fmt.Fprintln(os.Stdout, "ready")
		_ = os.Stdout.Sync()
		for {
			time.Sleep(time.Hour)
		}
	}

	root := t.TempDir()
	actualRoot := filepath.Join(root, "actual")
	if err := os.MkdirAll(actualRoot, 0700); err != nil {
		t.Fatal(err)
	}
	linkedRoot := filepath.Join(root, "home")
	if err := os.Symlink(actualRoot, linkedRoot); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(linkedRoot, "wsl")
	edgeBinary := filepath.Join(stateDir, "bin", "gohere-edge")
	if err := os.MkdirAll(filepath.Dir(edgeBinary), 0700); err != nil {
		t.Fatal(err)
	}
	current, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(edgeBinary, data, 0755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(edgeBinary, "-test.run=^TestInspectRecoversVerifiedLegacyEdgeIdentity$")
	cmd.Env = append(os.Environ(), "GOHERE_LEGACY_EDGE_HELPER=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "ready" {
		t.Fatalf("legacy helper did not start: output=%q err=%v", scanner.Text(), scanner.Err())
	}
	identity, ok := processIdentity(cmd.Process.Pid)
	if !ok {
		t.Fatal("legacy helper process identity unavailable")
	}
	record := edgeLockRecord{PID: cmd.Process.Pid, ProcessIdentity: identity}
	lockData, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "edge.lock"), lockData, 0600); err != nil {
		t.Fatal(err)
	}

	info, running, err := Inspect(stateDir)
	if err != nil || !running {
		t.Fatalf("inspect running = %v, err = %v", running, err)
	}
	wantBinary, err := filepath.EvalSymlinks(edgeBinary)
	if err != nil {
		t.Fatal(err)
	}
	wantHash, err := fileSHA256Hex(wantBinary)
	if err != nil {
		t.Fatal(err)
	}
	if info.EdgeBinary != wantBinary || info.EdgeSHA256 != wantHash || info.PID != cmd.Process.Pid {
		t.Fatalf("legacy running info = %#v", info)
	}
}

func TestInspectRejectsTamperedLiveEdgeIdentity(t *testing.T) {
	stateDir := t.TempDir()
	lockPath := filepath.Join(stateDir, "edge.lock")
	identity, ok := processIdentity(os.Getpid())
	if !ok {
		t.Skip("process identity unavailable")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	record := edgeLockRecord{
		PID:               os.Getpid(),
		ProcessIdentity:   identity,
		EdgeBinary:       executable,
		EdgeSHA256:       strings.Repeat("0", 64),
		CompanionBinary: `C:\\Temp\\gohere.exe`,
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, running, err := Inspect(stateDir); err == nil || running {
		t.Fatalf("inspect running = %v, err = %v", running, err)
	}
	if err := Stop(stateDir); err == nil || !strings.Contains(err.Error(), "refusing to stop") {
		t.Fatalf("stop error = %v", err)
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
