package tunnel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/roie/gohere/internal/muxtransport"
)

const InternalCommand = "__tunnel"

type Target byte

const (
	TargetHTTP  Target = 1
	TargetHTTPS Target = 2
)

const (
	streamReady byte = iota
	streamForbidden
	streamDialFailed
)

var protocolPreface = []byte("GOHERE-TUNNEL\x00\x02\x00")

type DialContextFunc func(context.Context, string, string) (net.Conn, error)

type ServerConfig struct {
	HTTPS     bool
	Dial      DialContextFunc
	LogOutput io.Writer
}

type Client struct {
	session *muxtransport.Session
}

func NewClient(ctx context.Context, conn net.Conn, _ io.Writer) (*Client, error) {
	if conn == nil {
		return nil, errors.New("tunnel connection is required")
	}
	if err := clientHandshake(ctx, conn); err != nil {
		return nil, err
	}
	session, err := muxtransport.NewClient(conn)
	if err != nil {
		return nil, err
	}
	return &Client{session: session}, nil
}

func (c *Client) Open(ctx context.Context, target Target) (net.Conn, error) {
	if c == nil || c.session == nil || c.session.IsClosed() {
		return nil, errors.New("tunnel session is closed")
	}
	stream, err := c.session.OpenStream(ctx)
	if err != nil {
		return nil, err
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = stream.Close()
		}
	}()
	stopContext := context.AfterFunc(ctx, func() { _ = stream.Close() })
	defer stopContext()
	deadline := time.Now().Add(10 * time.Second)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := stream.SetDeadline(deadline); err != nil {
		return nil, err
	}
	if _, err := stream.Write([]byte{byte(target)}); err != nil {
		return nil, contextError(ctx, err)
	}
	var status [1]byte
	if _, err := io.ReadFull(stream, status[:]); err != nil {
		return nil, contextError(ctx, err)
	}
	if !stopContext() {
		return nil, contextError(ctx, io.ErrClosedPipe)
	}
	if err := stream.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	switch status[0] {
	case streamReady:
		closeOnError = false
		return stream, nil
	case streamForbidden:
		return nil, fmt.Errorf("Windows tunnel rejected target %d", target)
	case streamDialFailed:
		return nil, fmt.Errorf("Windows tunnel could not connect target %d", target)
	default:
		return nil, fmt.Errorf("Windows tunnel returned invalid stream status %d", status[0])
	}
}

func contextError(ctx context.Context, fallback error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return fallback
}

func (c *Client) Close() error {
	if c == nil || c.session == nil {
		return nil
	}
	return c.session.Close()
}

func (c *Client) Done() <-chan struct{} {
	if c == nil || c.session == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	return c.session.Done()
}

func Serve(ctx context.Context, conn net.Conn, cfg ServerConfig) error {
	if conn == nil {
		return errors.New("tunnel connection is required")
	}
	if cfg.Dial == nil {
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		cfg.Dial = dialer.DialContext
	}
	if err := serverHandshake(ctx, conn); err != nil {
		return err
	}
	session, err := muxtransport.NewServer(conn)
	if err != nil {
		return err
	}
	defer session.Close()
	go func() {
		select {
		case <-ctx.Done():
			_ = session.Close()
		case <-session.Done():
		}
	}()

	for {
		stream, err := session.AcceptStream()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		go serveStream(ctx, stream, cfg)
	}
}

func serveStream(ctx context.Context, stream net.Conn, cfg ServerConfig) {
	defer stream.Close()
	_ = stream.SetReadDeadline(time.Now().Add(10 * time.Second))
	var requested [1]byte
	if _, err := io.ReadFull(stream, requested[:]); err != nil {
		return
	}
	_ = stream.SetReadDeadline(time.Time{})
	address, allowed := targetAddress(Target(requested[0]), cfg.HTTPS)
	if !allowed {
		_, _ = stream.Write([]byte{streamForbidden})
		return
	}
	upstream, err := cfg.Dial(ctx, "tcp", address)
	if err != nil {
		_, _ = stream.Write([]byte{streamDialFailed})
		return
	}
	defer upstream.Close()
	if _, err := stream.Write([]byte{streamReady}); err != nil {
		return
	}
	proxyDuplex(stream, upstream)
}

func targetAddress(target Target, https bool) (string, bool) {
	switch target {
	case TargetHTTP:
		return "127.0.0.1:80", true
	case TargetHTTPS:
		return "127.0.0.1:443", https
	default:
		return "", false
	}
}

func proxyDuplex(left, right net.Conn) {
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

func clientHandshake(ctx context.Context, conn net.Conn) error {
	defer boundHandshake(ctx, conn)()
	if _, err := conn.Write(protocolPreface); err != nil {
		return err
	}
	response := make([]byte, len(protocolPreface))
	if _, err := io.ReadFull(conn, response); err != nil {
		return err
	}
	if !bytes.Equal(response, protocolPreface) {
		return errors.New("incompatible Windows tunnel handshake")
	}
	return nil
}

func serverHandshake(ctx context.Context, conn net.Conn) error {
	defer boundHandshake(ctx, conn)()
	request := make([]byte, len(protocolPreface))
	if _, err := io.ReadFull(conn, request); err != nil {
		return err
	}
	if !bytes.Equal(request, protocolPreface) {
		return errors.New("invalid WSL tunnel handshake")
	}
	_, err := conn.Write(protocolPreface)
	return err
}

func boundHandshake(ctx context.Context, conn net.Conn) func() {
	deadline := time.Now().Add(10 * time.Second)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	_ = conn.SetDeadline(deadline)
	timer := time.AfterFunc(time.Until(deadline), func() { _ = conn.Close() })
	stopContext := context.AfterFunc(ctx, func() { _ = conn.Close() })
	return func() {
		timer.Stop()
		stopContext()
		_ = conn.SetDeadline(time.Time{})
	}
}
