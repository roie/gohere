package probe

import (
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

const (
	StatusReady   = "ready"
	StatusDead    = "dead"
	StatusUnknown = "unknown"

	DefaultTimeout = 500 * time.Millisecond
)

func TargetStatus(target string) string {
	return TargetStatusWithTimeout(target, DefaultTimeout)
}

func TargetStatusWithTimeout(target string, timeout time.Duration) string {
	resp, err := Head(target, timeout)
	if err != nil {
		if IsDefinitiveConnectionFailure(err) {
			return StatusDead
		}
		return StatusUnknown
	}
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	return StatusReady
}

func Head(target string, timeout time.Duration) (*http.Response, error) {
	transport := &http.Transport{
		DialContext: (&net.Dialer{Timeout: timeout}).DialContext,
	}
	client := http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Head(target)
	if err != nil {
		transport.CloseIdleConnections()
		return nil, err
	}
	if resp == nil || resp.Body == nil {
		transport.CloseIdleConnections()
		return resp, nil
	}
	resp.Body = transportClosingBody{ReadCloser: resp.Body, transport: transport}
	return resp, nil
}

type transportClosingBody struct {
	io.ReadCloser
	transport *http.Transport
}

func (b transportClosingBody) Close() error {
	err := b.ReadCloser.Close()
	b.transport.CloseIdleConnections()
	return err
}

func IsDefinitiveConnectionFailure(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.Errno(10061)) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "connection refused") ||
		strings.Contains(message, "actively refused")
}
