package probe

import (
	"net"
	"net/url"
	"os"
	"syscall"
	"testing"
)

func TestIsDefinitiveConnectionFailureDetectsWindowsRefused(t *testing.T) {
	err := &url.Error{
		Op:  "Head",
		URL: "http://127.0.0.1:1",
		Err: &net.OpError{
			Op:  "dial",
			Net: "tcp",
			Err: &os.SyscallError{
				Syscall: "connectex",
				Err:     syscall.Errno(10061),
			},
		},
	}

	if !IsDefinitiveConnectionFailure(err) {
		t.Fatal("expected Windows connection refused to be definitive")
	}
}
