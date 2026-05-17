package router

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
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
}

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

	token, err := EnsureToken(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	store := NewRouteStore(filepath.Join(cfg.StateDir, "routes.json"))
	server := NewServer(Config{Token: token, Store: store})

	httpLn, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return nil, err
	}
	adminLn, err := net.Listen("tcp", cfg.AdminAddr)
	if err != nil {
		httpLn.Close()
		return nil, err
	}

	running := &Running{
		HTTPAddr:    httpLn.Addr().String(),
		AdminAddr:   adminLn.Addr().String(),
		httpServer:  &http.Server{Handler: server.HTTPHandler()},
		adminServer: &http.Server{Handler: server.AdminHandler()},
		httpLn:      httpLn,
		adminLn:     adminLn,
	}
	go running.httpServer.Serve(httpLn)
	go running.adminServer.Serve(adminLn)
	go func() {
		<-ctx.Done()
		running.Close()
	}()
	return running, nil
}

func (r *Running) Close() error {
	if r == nil {
		return nil
	}
	if r.httpServer != nil {
		r.httpServer.Close()
	}
	if r.adminServer != nil {
		r.adminServer.Close()
	}
	if r.httpLn != nil {
		r.httpLn.Close()
	}
	if r.adminLn != nil {
		r.adminLn.Close()
	}
	return nil
}

func DefaultStateDir() string {
	return filepath.Join(homeDir(), ".gohere")
}

func homeDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return "."
}

func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
