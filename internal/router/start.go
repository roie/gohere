package router

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/roie/gohere/internal/userpath"
)

type StartConfig struct {
	HTTPAddr  string
	HTTPSAddr string
	AdminAddr string
	StateDir  string
	LogPath   string
	TLSConfig *tls.Config
}

type Running struct {
	HTTPAddr  string
	HTTPSAddr string
	AdminAddr string

	httpServer  *http.Server
	httpsServer *http.Server
	adminServer *http.Server
	httpLn      net.Listener
	httpsLn     net.Listener
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
		cfg.HTTPAddr = defaultHTTPAddrForGOOS(runtime.GOOS)
	}
	if cfg.TLSConfig != nil && cfg.HTTPSAddr == "" {
		cfg.HTTPSAddr = defaultHTTPSAddrForGOOS(runtime.GOOS)
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

	httpLn, err := listenHTTPForGOOS(runtime.GOOS, cfg.HTTPAddr)
	if err != nil {
		logFile.Close()
		return nil, listenError(cfg.HTTPAddr, err)
	}
	var httpsLn net.Listener
	if cfg.TLSConfig != nil {
		httpsLn, err = listenHTTPForGOOS(runtime.GOOS, cfg.HTTPSAddr)
		if err != nil {
			httpLn.Close()
			logFile.Close()
			return nil, listenError(cfg.HTTPSAddr, err)
		}
	}
	adminLn, err := net.Listen("tcp", cfg.AdminAddr)
	if err != nil {
		httpLn.Close()
		if httpsLn != nil {
			httpsLn.Close()
		}
		logFile.Close()
		return nil, err
	}
	pidPath := filepath.Join(cfg.StateDir, "router.pid")
	if err := writeRouterPID(pidPath); err != nil {
		httpLn.Close()
		if httpsLn != nil {
			httpsLn.Close()
		}
		adminLn.Close()
		logFile.Close()
		return nil, err
	}
	httpsAddr := ""
	if httpsLn != nil {
		httpsAddr = httpsLn.Addr().String()
	}
	fmt.Fprintf(logFile, "gohere router started http=%s https=%s admin=%s\n", httpLn.Addr().String(), httpsAddr, adminLn.Addr().String())

	proxyHandler := server.HTTPHandler()

	running = &Running{
		HTTPAddr:  httpLn.Addr().String(),
		HTTPSAddr: httpsAddr,
		AdminAddr: adminLn.Addr().String(),
		httpServer: &http.Server{
			Handler:           proxyHandler,
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
		httpsLn: httpsLn,
		adminLn: adminLn,
		pidPath: pidPath,
		logFile: logFile,
		done:    make(chan struct{}),
	}
	if cfg.TLSConfig != nil {
		running.httpsServer = &http.Server{
			Handler:           proxyHandler,
			TLSConfig:         preparedTLSConfig(cfg.TLSConfig),
			ReadHeaderTimeout: proxyReadHeaderTimeout,
		}
	}
	go running.httpServer.Serve(httpLn)
	if running.httpsServer != nil {
		go running.httpsServer.Serve(tls.NewListener(httpsLn, running.httpsServer.TLSConfig))
	}
	go running.adminServer.Serve(adminLn)
	go func() {
		<-ctx.Done()
		running.Close()
	}()
	return running, nil
}

func defaultHTTPAddrForGOOS(goos string) string {
	if goos == "darwin" {
		return "[::]:80"
	}
	return "127.0.0.1:80"
}

func defaultHTTPSAddrForGOOS(goos string) string {
	if goos == "darwin" {
		return "[::]:443"
	}
	return "127.0.0.1:443"
}

func preparedTLSConfig(cfg *tls.Config) *tls.Config {
	clone := cfg.Clone()
	if clone.MinVersion == 0 {
		clone.MinVersion = tls.VersionTLS12
	}
	return clone
}

func listenHTTPForGOOS(goos, addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	if goos == "darwin" && addr == "[::]:80" {
		return loopbackOnlyListener{Listener: ln}, nil
	}
	return ln, nil
}

type loopbackOnlyListener struct {
	net.Listener
}

func (l loopbackOnlyListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		remote, ok := conn.RemoteAddr().(*net.TCPAddr)
		if ok && remote.IP.IsLoopback() {
			return conn, nil
		}
		_ = conn.Close()
	}
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
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.EADDRINUSE) {
		return true
	}
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
	if r.httpsServer != nil {
		if err := r.httpsServer.Shutdown(ctx); err != nil && firstErr == nil {
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
	if r.httpsLn != nil {
		r.httpsLn.Close()
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
	tmp, err := os.CreateTemp(filepath.Dir(pidPath), filepath.Base(pidPath)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write([]byte(strconv.Itoa(os.Getpid()) + "\n")); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := replaceFile(tmpPath, pidPath); err != nil {
		return err
	}
	if err := os.Chmod(pidPath, 0600); err != nil {
		return err
	}
	cleanup = false
	return nil
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
