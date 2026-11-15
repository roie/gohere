package router

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"time"
)

type proxyUpload struct {
	io.ReadCloser
	controller *http.ResponseController
	complete   atomic.Bool
	connection atomic.Pointer[uploadConn]
}

func (body *proxyUpload) Read(p []byte) (int, error) {
	n, err := body.ReadCloser.Read(p)
	if err == io.EOF {
		body.complete.Store(true)
	}
	return n, err
}

func (body *proxyUpload) Close() error {
	return errors.Join(body.interrupt(), body.ReadCloser.Close())
}

func (body *proxyUpload) interrupt() error {
	if body.complete.Load() {
		return nil
	}
	return body.controller.SetReadDeadline(time.Now())
}

func (body *proxyUpload) gotConn(info httptrace.GotConnInfo) {
	conn := info.Conn
	if secure, ok := conn.(*tls.Conn); ok {
		if secure.ConnectionState().NegotiatedProtocol == "h2" {
			return
		}
		conn = secure.NetConn()
	}
	if conn, ok := conn.(*uploadConn); ok {
		body.connection.Store(conn)
		conn.upload.Store(body)
		if conn.closed.Load() {
			body.interrupt()
		}
	}
}

func (body *proxyUpload) detach() {
	if conn := body.connection.Load(); conn != nil {
		conn.upload.CompareAndSwap(body, nil)
	}
}

type uploadConn struct {
	net.Conn
	upload atomic.Pointer[proxyUpload]
	closed atomic.Bool
}

func (conn *uploadConn) Close() error {
	conn.closed.Store(true)
	var err error
	if body := conn.upload.Swap(nil); body != nil {
		err = body.interrupt()
	}
	return errors.Join(err, conn.Conn.Close())
}

func (conn *uploadConn) CloseWrite() error {
	if conn, ok := conn.Conn.(interface{ CloseWrite() error }); ok {
		return conn.CloseWrite()
	}
	return http.ErrNotSupported
}
