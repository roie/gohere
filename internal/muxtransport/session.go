package muxtransport

import (
	"context"
	"errors"
	"io"
	"net"
	"time"

	yamux "github.com/libp2p/go-yamux/v5"
)

type Session struct {
	inner *yamux.Session
}

func NewClient(conn net.Conn) (*Session, error) {
	if conn == nil {
		return nil, errors.New("multiplexed transport connection is required")
	}
	inner, err := yamux.Client(conn, newConfig(), nil)
	if err != nil {
		return nil, err
	}
	return &Session{inner: inner}, nil
}

func NewServer(conn net.Conn) (*Session, error) {
	if conn == nil {
		return nil, errors.New("multiplexed transport connection is required")
	}
	inner, err := yamux.Server(conn, newConfig(), nil)
	if err != nil {
		return nil, err
	}
	return &Session{inner: inner}, nil
}

func (s *Session) OpenStream(ctx context.Context) (net.Conn, error) {
	if s == nil || s.inner == nil || s.inner.IsClosed() {
		return nil, errors.New("multiplexed transport session is closed")
	}
	if ctx == nil {
		return nil, errors.New("multiplexed transport context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.inner.OpenStream(ctx)
}

func (s *Session) AcceptStream() (net.Conn, error) {
	if s == nil || s.inner == nil || s.inner.IsClosed() {
		return nil, errors.New("multiplexed transport session is closed")
	}
	return s.inner.AcceptStream()
}

func (s *Session) Close() error {
	if s == nil || s.inner == nil {
		return nil
	}
	return s.inner.Close()
}

func (s *Session) Done() <-chan struct{} {
	if s == nil || s.inner == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	return s.inner.CloseChan()
}

func (s *Session) IsClosed() bool {
	return s == nil || s.inner == nil || s.inner.IsClosed()
}

func newConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	cfg.EnableKeepAlive = true
	cfg.KeepAliveInterval = 10 * time.Second
	cfg.ConnectionWriteTimeout = 15 * time.Second
	cfg.MaxStreamWindowSize = 256 * 1024
	cfg.AcceptBacklog = 256
	return cfg
}
