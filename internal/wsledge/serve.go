package wsledge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/roie/gohere/internal/tunnel"
)

const InternalCommand = "__edge"

var ErrAlreadyRunning = errors.New("WSL loopback edge is already running")

type StreamSession interface {
	Open(context.Context, tunnel.Target) (net.Conn, error)
	Close() error
	Done() <-chan struct{}
}

type SessionStarter func(context.Context) (StreamSession, error)
type ListenFunc func(string, string) (net.Listener, error)

type Config struct {
	StateDir        string
	CompanionBinary string
	HTTPS           bool
	Listen          ListenFunc
	StartSession    SessionStarter
	LogOutput       io.Writer
}

type edgeListener struct {
	listener net.Listener
	target   tunnel.Target
}

func Serve(ctx context.Context, cfg Config) error {
	if cfg.StateDir == "" {
		return errors.New("WSL edge state directory is required")
	}
	if cfg.Listen == nil {
		cfg.Listen = net.Listen
	}
	if cfg.LogOutput == nil {
		cfg.LogOutput = io.Discard
	}
	if cfg.StartSession == nil {
		if cfg.CompanionBinary == "" {
			return errors.New("Windows tunnel companion path is required")
		}
		cfg.StartSession = func(ctx context.Context) (StreamSession, error) {
			return startHelperSession(ctx, cfg.CompanionBinary, cfg.LogOutput)
		}
	}
	edgeBinary, err := os.Executable()
	if err != nil {
		return err
	}
	edgeBinary, err = filepath.EvalSymlinks(edgeBinary)
	if err != nil {
		return err
	}
	edgeHash, err := fileSHA256Hex(edgeBinary)
	if err != nil {
		return err
	}
	lock, err := acquireEdgeLock(filepath.Join(cfg.StateDir, "edge.lock"), edgeBinary, edgeHash, cfg.CompanionBinary)
	if err != nil {
		if errors.Is(err, ErrAlreadyRunning) {
			return nil
		}
		return err
	}
	defer lock.Release()

	listeners, err := openEdgeListeners(cfg)
	if err != nil {
		return err
	}
	defer func() {
		for _, listener := range listeners {
			_ = listener.listener.Close()
		}
	}()

	manager := &sessionManager{start: cfg.StartSession}
	defer manager.Close()
	errCh := make(chan error, len(listeners))
	for _, listener := range listeners {
		listener := listener
		go acceptConnections(ctx, listener, manager, errCh)
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	}
}

func openEdgeListeners(cfg Config) ([]edgeListener, error) {
	addresses := []struct {
		address string
		target  tunnel.Target
	}{
		{address: "127.0.0.1:80", target: tunnel.TargetHTTP},
		{address: "[::1]:80", target: tunnel.TargetHTTP},
	}
	if cfg.HTTPS {
		addresses = append(addresses,
			struct {
				address string
				target  tunnel.Target
			}{address: "127.0.0.1:443", target: tunnel.TargetHTTPS},
			struct {
				address string
				target  tunnel.Target
			}{address: "[::1]:443", target: tunnel.TargetHTTPS},
		)
	}
	listeners := make([]edgeListener, 0, len(addresses))
	for _, item := range addresses {
		listener, err := cfg.Listen("tcp", item.address)
		if err != nil {
			for _, opened := range listeners {
				_ = opened.listener.Close()
			}
			return nil, fmt.Errorf("WSL loopback edge cannot listen on %s: %w", item.address, err)
		}
		listeners = append(listeners, edgeListener{listener: listener, target: item.target})
	}
	return listeners, nil
}

func acceptConnections(ctx context.Context, listener edgeListener, manager *sessionManager, errCh chan<- error) {
	for {
		connection, err := listener.listener.Accept()
		if err != nil {
			select {
			case errCh <- err:
			default:
			}
			return
		}
		go handleConnection(ctx, connection, listener.target, manager)
	}
}

func handleConnection(ctx context.Context, local net.Conn, target tunnel.Target, manager *sessionManager) {
	defer local.Close()
	session, err := manager.Get(ctx)
	if err != nil {
		return
	}
	remote, err := session.Open(ctx, target)
	if err != nil {
		select {
		case <-session.Done():
			manager.Invalidate(session)
		default:
		}
		return
	}
	defer remote.Close()
	proxyEdgeDuplex(local, remote)
}

func proxyEdgeDuplex(left, right net.Conn) {
	done := make(chan struct{}, 2)
	copyOneWay := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if closeWriter, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = closeWriter.CloseWrite()
		}
		done <- struct{}{}
	}
	go copyOneWay(right, left)
	go copyOneWay(left, right)
	<-done
	<-done
}

type sessionManager struct {
	mu          sync.Mutex
	start       SessionStarter
	session     StreamSession
	failures    int
	nextAttempt time.Time
}

func (m *sessionManager) Get(ctx context.Context) (StreamSession, error) {
	for {
		m.mu.Lock()
		if m.session != nil {
			select {
			case <-m.session.Done():
				_ = m.session.Close()
				m.session = nil
			default:
				session := m.session
				m.mu.Unlock()
				return session, nil
			}
		}
		wait := time.Until(m.nextAttempt)
		if wait > 0 {
			m.mu.Unlock()
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
			continue
		}
		session, err := m.start(ctx)
		if err != nil {
			m.failures++
			m.nextAttempt = time.Now().Add(reconnectDelay(m.failures))
			m.mu.Unlock()
			return nil, err
		}
		m.session = session
		m.failures = 0
		m.nextAttempt = time.Time{}
		m.mu.Unlock()
		return session, nil
	}
}

func (m *sessionManager) Invalidate(session StreamSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.session == session {
		_ = m.session.Close()
		m.session = nil
	}
}

func (m *sessionManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.session != nil {
		_ = m.session.Close()
		m.session = nil
	}
}

func reconnectDelay(failures int) time.Duration {
	if failures < 1 {
		return 0
	}
	delay := 100 * time.Millisecond
	for i := 1; i < failures && delay < 5*time.Second; i++ {
		delay *= 2
	}
	if delay > 5*time.Second {
		return 5 * time.Second
	}
	return delay
}

func startHelperSession(ctx context.Context, binary string, logOutput io.Writer) (StreamSession, error) {
	command := exec.CommandContext(ctx, binary, tunnel.InternalCommand)
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	command.Stderr = logOutput
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, err
	}
	closer := &helperProcessCloser{command: command, stdin: stdin, stdout: stdout}
	connection := &tunnel.PipeConn{Reader: stdout, Writer: stdin, Closer: closer}
	client, err := tunnel.NewClient(ctx, connection, logOutput)
	if err != nil {
		_ = closer.Close()
		_ = command.Wait()
		return nil, err
	}
	go func() {
		_ = command.Wait()
		_ = client.Close()
	}()
	return client, nil
}

type helperProcessCloser struct {
	command *exec.Cmd
	stdin   io.Closer
	stdout  io.Closer
	once    sync.Once
}

func (c *helperProcessCloser) Close() error {
	var closeErr error
	c.once.Do(func() {
		if c.stdin != nil {
			closeErr = errors.Join(closeErr, c.stdin.Close())
		}
		if c.stdout != nil {
			closeErr = errors.Join(closeErr, c.stdout.Close())
		}
		if c.command != nil && c.command.Process != nil {
			_ = c.command.Process.Kill()
		}
	})
	return closeErr
}

type edgeLock struct {
	path   string
	record edgeLockRecord
}

type RunningInfo struct {
	PID             int
	ProcessIdentity string
	EdgeBinary      string
	EdgeSHA256      string
	CompanionBinary string
}

type edgeLockRecord struct {
	PID             int    `json:"pid"`
	ProcessIdentity string `json:"processIdentity"`
	EdgeBinary      string `json:"edgeBinary,omitempty"`
	EdgeSHA256      string `json:"edgeSha256,omitempty"`
	CompanionBinary string `json:"companionBinary,omitempty"`
}

func acquireEdgeLock(path, edgeBinary, edgeHash, companionBinary string) (*edgeLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			pid := os.Getpid()
			identity, ok := processIdentity(pid)
			if !ok {
				_ = file.Close()
				_ = os.Remove(path)
				return nil, errors.New("could not identify the WSL edge process")
			}
			record := edgeLockRecord{
				PID:             pid,
				ProcessIdentity: identity,
				EdgeBinary:      edgeBinary,
				EdgeSHA256:      edgeHash,
				CompanionBinary: companionBinary,
			}
			if writeErr := json.NewEncoder(file).Encode(record); writeErr != nil {
				_ = file.Close()
				_ = os.Remove(path)
				return nil, writeErr
			}
			if syncErr := file.Sync(); syncErr != nil {
				_ = file.Close()
				_ = os.Remove(path)
				return nil, syncErr
			}
			if closeErr := file.Close(); closeErr != nil {
				_ = os.Remove(path)
				return nil, closeErr
			}
			return &edgeLock{path: path, record: record}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		record, readErr := readEdgeLock(path)
		if readErr == nil && record.ProcessIdentity == "" && processAlive(record.PID) {
			return nil, errors.New("existing WSL edge lock belongs to a live process but has no verifiable identity")
		}
		if readErr == nil && edgeProcessOwnerMatches(record) {
			return nil, ErrAlreadyRunning
		}
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return nil, removeErr
		}
	}
	return nil, ErrAlreadyRunning
}

func (l *edgeLock) Release() {
	if l == nil {
		return
	}
	record, err := readEdgeLock(l.path)
	if err == nil && record == l.record {
		_ = os.Remove(l.path)
	}
}

func Running(stateDir string) bool {
	record, err := readEdgeLock(filepath.Join(stateDir, "edge.lock"))
	return err == nil && edgeProcessOwnerMatches(record)
}

func Inspect(stateDir string) (RunningInfo, bool, error) {
	record, err := readEdgeLock(filepath.Join(stateDir, "edge.lock"))
	if errors.Is(err, os.ErrNotExist) {
		return RunningInfo{}, false, nil
	}
	if err != nil {
		return RunningInfo{}, false, err
	}
	if !edgeProcessOwnerMatches(record) {
		return RunningInfo{}, false, nil
	}
	info, err := verifiedRunningInfo(stateDir, record)
	if err != nil {
		return RunningInfo{}, false, err
	}
	return info, true, nil
}

func Stop(stateDir string) error {
	lockPath := filepath.Join(stateDir, "edge.lock")
	record, err := readEdgeLock(lockPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if record.ProcessIdentity == "" {
		if !processAlive(record.PID) {
			return os.Remove(lockPath)
		}
		return errors.New("refusing to stop a live process from an unverifiable legacy WSL edge lock")
	}
	if !edgeProcessOwnerMatches(record) {
		return os.Remove(lockPath)
	}
	if _, err := verifiedRunningInfo(stateDir, record); err != nil {
		return fmt.Errorf("refusing to stop a live WSL edge whose binary identity cannot be verified: %w", err)
	}
	if err := stopProcess(record.PID); err != nil {
		return err
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(record.PID) {
			_ = os.Remove(lockPath)
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("WSL loopback edge process %d did not stop", record.PID)
}

func readEdgeLock(path string) (edgeLockRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return edgeLockRecord{}, err
	}
	var record edgeLockRecord
	if err := json.Unmarshal(data, &record); err == nil && record.PID > 0 {
		return record, nil
	}
	pid, err := strconv.Atoi(string(bytesTrimSpace(data)))
	if err != nil || pid <= 0 {
		return edgeLockRecord{}, errors.New("invalid WSL edge lock")
	}
	return edgeLockRecord{PID: pid}, nil
}

func edgeProcessOwnerMatches(record edgeLockRecord) bool {
	if record.PID <= 0 || record.ProcessIdentity == "" || !processAlive(record.PID) {
		return false
	}
	identity, ok := processIdentity(record.PID)
	return ok && identity == record.ProcessIdentity
}

func edgeProcessMatches(record edgeLockRecord) bool {
	if !edgeProcessOwnerMatches(record) {
		return false
	}
	executable, ok := processExecutable(record.PID)
	if !ok || executable != record.EdgeBinary {
		return false
	}
	hash, err := fileSHA256Hex(executable)
	return err == nil && hash == record.EdgeSHA256
}

func verifiedRunningInfo(stateDir string, record edgeLockRecord) (RunningInfo, error) {
	executable, ok := processExecutable(record.PID)
	if !ok {
		return RunningInfo{}, errors.New("running edge executable is unavailable")
	}
	edgeBinary := record.EdgeBinary
	if edgeBinary == "" {
		edgeBinary = executable
	}
	if executable != edgeBinary {
		return RunningInfo{}, errors.New("running edge executable path does not match its lock record")
	}
	binDir, err := filepath.EvalSymlinks(filepath.Join(stateDir, "bin"))
	if err != nil {
		return RunningInfo{}, errors.New("WSL edge integration bin directory is unavailable")
	}
	relative, err := filepath.Rel(binDir, edgeBinary)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) || !strings.HasPrefix(filepath.Base(edgeBinary), "gohere-edge") {
		return RunningInfo{}, errors.New("running edge executable is outside the integration bin directory")
	}
	hash, err := fileSHA256Hex(edgeBinary)
	if err != nil {
		return RunningInfo{}, err
	}
	if record.EdgeSHA256 != "" && record.EdgeSHA256 != hash {
		return RunningInfo{}, errors.New("running edge executable hash does not match its lock record")
	}
	companionBinary := record.CompanionBinary
	if companionBinary == "" {
		if args, ok := processArguments(record.PID); ok && len(args) == 3 && args[1] == InternalCommand {
			companionBinary = args[2]
		}
	}
	return RunningInfo{
		PID:             record.PID,
		ProcessIdentity: record.ProcessIdentity,
		EdgeBinary:      edgeBinary,
		EdgeSHA256:      hash,
		CompanionBinary: companionBinary,
	}, nil
}

func fileSHA256Hex(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func bytesTrimSpace(data []byte) []byte {
	start := 0
	for start < len(data) && (data[start] == ' ' || data[start] == '\n' || data[start] == '\r' || data[start] == '\t') {
		start++
	}
	end := len(data)
	for end > start && (data[end-1] == ' ' || data[end-1] == '\n' || data[end-1] == '\r' || data[end-1] == '\t') {
		end--
	}
	return data[start:end]
}
