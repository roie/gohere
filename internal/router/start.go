package router

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/roie/gohere/internal/userpath"
)

type StartConfig struct {
	HTTPAddr  string
	AdminAddr string
	StateDir  string
	LogPath   string
}

type Running struct {
	HTTPAddr  string
	AdminAddr string

	httpServer  *http.Server
	adminServer *http.Server
	httpLn      net.Listener
	adminLn     net.Listener
	pidPath     string
	logFile     *os.File
	done        chan struct{}
	doneOnce    sync.Once
}

const (
	proxyReadHeaderTimeout = 5 * time.Second
	adminReadHeaderTimeout = 5 * time.Second
	adminReadTimeout       = 10 * time.Second
	adminWriteTimeout      = 10 * time.Second
	adminIdleTimeout       = 60 * time.Second
)

func Start(ctx context.Context, cfg StartConfig) (*Running, error) {
	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = "127.0.0.1:80"
	}
	if cfg.AdminAddr == "" {
		cfg.AdminAddr = "127.0.0.1:39399"
	}
	if !isLoopbackAddr(cfg.AdminAddr) {
		return nil, fmt.Errorf("admin API must listen on loopback, got %s", cfg.AdminAddr)
	}
	if cfg.StateDir == "" {
		cfg.StateDir = DefaultStateDir()
	}
	if cfg.LogPath == "" {
		cfg.LogPath = filepath.Join(cfg.StateDir, "logs", "router.log")
	}
	if cfg.LogPath != "" {
		if err := RotateLog(cfg.LogPath); err != nil {
			return nil, err
		}
	}
	logFile, err := openRouterLog(cfg.LogPath)
	if err != nil {
		return nil, err
	}

	token, err := EnsureToken(cfg.StateDir)
	if err != nil {
		logFile.Close()
		return nil, err
	}
	store := NewRouteStore(filepath.Join(cfg.StateDir, RoutesFilename))
	var running *Running
	server := NewServer(Config{Token: token, Store: store, Shutdown: func() {
		if running != nil {
			running.Close()
		}
	}})

	httpLn, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		logFile.Close()
		return nil, listenError(cfg.HTTPAddr, err)
	}
	adminLn, err := net.Listen("tcp", cfg.AdminAddr)
	if err != nil {
		httpLn.Close()
		logFile.Close()
		return nil, err
	}
	pidPath := filepath.Join(cfg.StateDir, "router.pid")
	if err := writeRouterPID(pidPath); err != nil {
		httpLn.Close()
		adminLn.Close()
		logFile.Close()
		return nil, err
	}
	fmt.Fprintf(logFile, "gohere router started http=%s admin=%s\n", httpLn.Addr().String(), adminLn.Addr().String())

	running = &Running{
		HTTPAddr:  httpLn.Addr().String(),
		AdminAddr: adminLn.Addr().String(),
		httpServer: &http.Server{
			Handler:           server.HTTPHandler(),
			ReadHeaderTimeout: proxyReadHeaderTimeout,
		},
		adminServer: &http.Server{
			Handler:           server.AdminHandler(),
			ReadHeaderTimeout: adminReadHeaderTimeout,
			ReadTimeout:       adminReadTimeout,
			WriteTimeout:      adminWriteTimeout,
			IdleTimeout:       adminIdleTimeout,
		},
		httpLn:  httpLn,
		adminLn: adminLn,
		pidPath: pidPath,
		logFile: logFile,
		done:    make(chan struct{}),
	}
	go running.httpServer.Serve(httpLn)
	go running.adminServer.Serve(adminLn)
	go func() {
		<-ctx.Done()
		running.Close()
	}()
	return running, nil
}

func listenError(addr string, err error) error {
	msg := fmt.Sprintf("gohere router cannot listen on %s", addr)
	if isAddressInUseError(err) {
		_, port, _ := net.SplitHostPort(addr)
		if owner := findPortOwner(port); owner != "" {
			return fmt.Errorf("%s: port is already in use; owning process: %s; stop that process and try again: %w", msg, owner, err)
		}
		return fmt.Errorf("%s: port is already in use; stop the process using that port and try again: %w", msg, err)
	}
	return fmt.Errorf("%s: %w", msg, err)
}

func isAddressInUseError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "address already in use") ||
		strings.Contains(msg, "only one usage of each socket address")
}

var findPortOwner = func(port string) string {
	if port == "" {
		return ""
	}
	if path, err := exec.LookPath("lsof"); err == nil {
		out, err := exec.Command(path, "-nP", "-iTCP:"+port, "-sTCP:LISTEN").CombinedOutput()
		if err == nil {
			return firstNonHeaderLine(string(out))
		}
	}
	if path, err := exec.LookPath("ss"); err == nil {
		out, err := exec.Command(path, "-ltnp", "sport", "="+port).CombinedOutput()
		if err == nil {
			return firstNonHeaderLine(string(out))
		}
	}
	return ""
}

func firstNonHeaderLine(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "COMMAND ") || strings.HasPrefix(line, "State ") {
			continue
		}
		return line
	}
	return ""
}

func (r *Running) Close() error {
	if r == nil {
		return nil
	}
	defer r.doneOnce.Do(func() {
		close(r.done)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var firstErr error
	if r.httpServer != nil {
		if err := r.httpServer.Shutdown(ctx); err != nil {
			firstErr = err
		}
	}
	if r.adminServer != nil {
		if err := r.adminServer.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if r.httpLn != nil {
		r.httpLn.Close()
	}
	if r.adminLn != nil {
		r.adminLn.Close()
	}
	if r.pidPath != "" {
		os.Remove(r.pidPath)
	}
	if r.logFile != nil {
		r.logFile.Close()
	}
	return firstErr
}

func (r *Running) Done() <-chan struct{} {
	if r == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	return r.done
}

func DefaultStateDir() string {
	return filepath.Join(userpath.HomeDir(), ".gohere")
}

func writeRouterPID(pidPath string) error {
	if err := os.MkdirAll(filepath.Dir(pidPath), 0700); err != nil {
		return err
	}
	return os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0600)
}

func openRouterLog(logPath string) (*os.File, error) {
	if logPath == "" {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0700); err != nil {
		return nil, err
	}
	return os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
}

func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
