//go:build darwin

package router

import (
	"errors"
	"io"
	"net"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestDarwinWildcardListenerAcceptsOnlyLoopback(t *testing.T) {
	listener, err := listenHTTPForGOOS("darwin", "[::]:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	bound, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address = %T, want *net.TCPAddr", listener.Addr())
	}
	accepted := make(chan net.Addr, 4)
	acceptErr := make(chan error, 1)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					select {
					case acceptErr <- err:
					default:
					}
				}
				return
			}
			accepted <- conn.RemoteAddr()
			_ = conn.Close()
		}
	}()

	assertDarwinAccepted(t, bound.Port, "127.0.0.1", accepted, acceptErr)
	assertDarwinAccepted(t, bound.Port, "::1", accepted, acceptErr)

	t.Run("non-loopback", func(t *testing.T) {
		conn, host, ok := dialDarwinNonLoopback(t, bound.Port)
		if !ok {
			t.Skip("no usable active non-loopback interface address")
		}
		defer conn.Close()
		if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		var one [1]byte
		if _, err := conn.Read(one[:]); err == nil {
			t.Fatalf("non-loopback connection through %s remained open", host)
		} else if !errors.Is(err, io.EOF) && !errors.Is(err, syscall.ECONNRESET) {
			t.Fatalf("non-loopback read through %s = %v, want EOF or reset", host, err)
		}

		select {
		case remote := <-accepted:
			t.Fatalf("non-loopback connection reached protocol boundary: %v", remote)
		case err := <-acceptErr:
			t.Fatal(err)
		case <-time.After(150 * time.Millisecond):
		}

		assertDarwinAccepted(t, bound.Port, "127.0.0.1", accepted, acceptErr)
	})
}

func assertDarwinAccepted(
	t *testing.T,
	port int,
	host string,
	accepted <-chan net.Addr,
	acceptErr <-chan error,
) {
	t.Helper()
	conn, err := net.DialTimeout(
		"tcp",
		net.JoinHostPort(host, strconv.Itoa(port)),
		time.Second,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	select {
	case remote := <-accepted:
		tcpRemote, ok := remote.(*net.TCPAddr)
		if !ok || !tcpRemote.IP.IsLoopback() {
			t.Fatalf("accepted remote = %v, want loopback", remote)
		}
	case err := <-acceptErr:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for accepted loopback %s", host)
	}
}

func dialDarwinNonLoopback(t *testing.T, port int) (net.Conn, string, bool) {
	t.Helper()
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	var hosts []string
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch value := addr.(type) {
			case *net.IPNet:
				ip = value.IP
			case *net.IPAddr:
				ip = value.IP
			}
			if ip == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() {
				continue
			}
			host := ip.String()
			if ip.To4() == nil && ip.IsLinkLocalUnicast() {
				host += "%" + iface.Name
			}
			if ip.To4() != nil {
				hosts = append([]string{host}, hosts...)
			} else {
				hosts = append(hosts, host)
			}
		}
	}

	for _, host := range hosts {
		conn, err := net.DialTimeout(
			"tcp",
			net.JoinHostPort(host, strconv.Itoa(port)),
			time.Second,
		)
		if err == nil {
			return conn, host, true
		}
	}
	return nil, "", false
}
