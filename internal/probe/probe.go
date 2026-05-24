package probe

import (
	"errors"
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
	client := http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{Timeout: timeout}).DialContext,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return client.Head(target)
}

func IsDefinitiveConnectionFailure(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.Errno(10061)) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "connection refused") ||
		strings.Contains(message, "actively refused")
}
