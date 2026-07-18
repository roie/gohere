//go:build windows

package lanmdns

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	windowsPacketBufferSize = 9000
	windowsControlSize      = 128
	windowsIOPollInterval   = 250 * time.Millisecond
)

type windowsTransport struct {
	spec      Interface
	conn      *net.UDPConn
	closeOnce sync.Once
	closeErr  error
}

// RunWindowsTransportSpike verifies the native transport without starting the mDNS protocol actor.
func RunWindowsTransportSpike(ctx context.Context, spec Interface) error {
	tr, err := newPlatformTransport(spec)
	if err != nil {
		return err
	}
	windowsTR := tr.(*windowsTransport)
	defer windowsTR.Close()

	multicastTTL, unicastTTL, err := windowsTR.socketTTLs()
	if err != nil {
		return err
	}
	if multicastTTL != 255 || unicastTTL != 255 {
		return fmt.Errorf("Windows LAN mDNS TTLs are multicast %d, unicast %d; want 255, 255", multicastTTL, unicastTTL)
	}

	payload := make([]byte, 32)
	if _, err := rand.Read(payload); err != nil {
		return fmt.Errorf("generate Windows LAN mDNS spike payload: %w", err)
	}
	if err := windowsTR.WritePacket(ctx, outboundPacket{Payload: payload, Destination: mdnsIPv4AddrPort}); err != nil {
		return err
	}
	for {
		packet, err := windowsTR.ReadPacket(ctx)
		if err != nil {
			return err
		}
		if bytes.Equal(packet.Payload, payload) {
			return nil
		}
	}
}

func newPlatformTransport(spec Interface) (transport, error) {
	if err := validateInterface(spec); err != nil {
		return nil, err
	}

	addr := spec.Prefix.Addr().As4()
	listenConfig := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
		var controlErr error
		if err := raw.Control(func(fd uintptr) {
			handle := windows.Handle(fd)
			for _, option := range []struct {
				level int
				name  int
				value int
			}{
				{windows.SOL_SOCKET, windows.SO_REUSEADDR, 1},
				{windows.IPPROTO_IP, windows.IP_PKTINFO, 1},
				{windows.IPPROTO_IP, windows.IP_MULTICAST_TTL, 255},
				{windows.IPPROTO_IP, windows.IP_TTL, 255},
				{windows.IPPROTO_IP, windows.IP_MULTICAST_LOOP, 1},
			} {
				if err := windows.SetsockoptInt(handle, option.level, option.name, option.value); err != nil {
					controlErr = err
					return
				}
			}
			if err := windows.SetsockoptInet4Addr(handle, windows.IPPROTO_IP, windows.IP_MULTICAST_IF, addr); err != nil {
				controlErr = err
			}
		}); err != nil {
			return err
		}
		return controlErr
	}}

	packetConn, err := listenConfig.ListenPacket(context.TODO(), "udp4", "0.0.0.0:5353")
	if err != nil {
		return nil, fmt.Errorf("listen for Windows LAN mDNS: %w", err)
	}
	conn, ok := packetConn.(*net.UDPConn)
	if !ok {
		closeErr := packetConn.Close()
		return nil, errors.Join(fmt.Errorf("Windows LAN mDNS listener is %T, want *net.UDPConn", packetConn), closeErr)
	}

	tr := &windowsTransport{spec: spec, conn: conn}
	if err := tr.withHandle(func(handle windows.Handle) error {
		return windows.SetsockoptIPMreq(handle, windows.IPPROTO_IP, windows.IP_ADD_MEMBERSHIP, &windows.IPMreq{
			Multiaddr: mdnsIPv4AddrPort.Addr().As4(),
			Interface: addr,
		})
	}); err != nil {
		closeErr := conn.Close()
		return nil, errors.Join(fmt.Errorf("join Windows LAN mDNS multicast group: %w", err), closeErr)
	}
	return tr, nil
}

func (t *windowsTransport) ReadPacket(ctx context.Context) (packet, error) {
	if err := ctx.Err(); err != nil {
		return packet{}, err
	}
	payload := make([]byte, windowsPacketBufferSize)
	control := make([]byte, windowsControlSize)
	for {
		if err := t.conn.SetReadDeadline(ioDeadline(ctx)); err != nil {
			return packet{}, err
		}
		n, controlN, flags, source, err := t.conn.ReadMsgUDPAddrPort(payload, control)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return packet{}, ctxErr
			}
			if errors.Is(err, os.ErrDeadlineExceeded) {
				continue
			}
			return packet{}, err
		}
		if flags&windows.MSG_TRUNC != 0 || flags&windows.MSG_CTRUNC != 0 {
			continue
		}
		destination, interfaceIndex, ok := parseWindowsPacketInfo(control[:controlN])
		if !ok || !acceptsInterface(t.spec, interfaceIndex) {
			continue
		}
		return packet{
			Payload:        append([]byte(nil), payload[:n]...),
			Source:         source,
			Destination:    netip.AddrPortFrom(destination, 5353),
			InterfaceIndex: interfaceIndex,
		}, nil
	}
}

func (t *windowsTransport) WritePacket(ctx context.Context, packet outboundPacket) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !packet.Destination.IsValid() || !packet.Destination.Addr().Is4() {
		return fmt.Errorf("Windows LAN mDNS destination must be IPv4")
	}
	if err := t.conn.SetWriteDeadline(ioDeadline(ctx)); err != nil {
		return err
	}
	_, err := t.conn.WriteToUDPAddrPort(packet.Payload, packet.Destination)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return err
	}
	return nil
}

func (t *windowsTransport) Close() error {
	t.closeOnce.Do(func() {
		t.closeErr = t.conn.Close()
	})
	return t.closeErr
}

func (t *windowsTransport) socketTTLs() (multicast, unicast int, err error) {
	err = t.withHandle(func(handle windows.Handle) error {
		var getErr error
		multicast, getErr = windows.GetsockoptInt(handle, windows.IPPROTO_IP, windows.IP_MULTICAST_TTL)
		if getErr != nil {
			return getErr
		}
		unicast, getErr = windows.GetsockoptInt(handle, windows.IPPROTO_IP, windows.IP_TTL)
		return getErr
	})
	return multicast, unicast, err
}

func (t *windowsTransport) withHandle(fn func(windows.Handle) error) error {
	raw, err := t.conn.SyscallConn()
	if err != nil {
		return err
	}
	var callErr error
	if err := raw.Control(func(fd uintptr) {
		callErr = fn(windows.Handle(fd))
	}); err != nil {
		return err
	}
	return callErr
}

func ioDeadline(ctx context.Context) time.Time {
	deadline := time.Now().Add(windowsIOPollInterval)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		return contextDeadline
	}
	return deadline
}

func parseWindowsPacketInfo(control []byte) (netip.Addr, int, bool) {
	const headerSize = int(unsafe.Sizeof(windows.WSACMSGHDR{}))
	const infoSize = int(unsafe.Sizeof(windows.IN_PKTINFO{}))
	for offset := 0; offset+headerSize <= len(control); {
		header := *(*windows.WSACMSGHDR)(unsafe.Pointer(&control[offset]))
		length := int(header.Len)
		if length < headerSize || offset+length > len(control) {
			return netip.Addr{}, 0, false
		}
		dataOffset := offset + cmsgAlign(headerSize)
		if header.Level == windows.IPPROTO_IP && header.Type == windows.IP_PKTINFO && dataOffset+infoSize <= offset+length {
			info := *(*windows.IN_PKTINFO)(unsafe.Pointer(&control[dataOffset]))
			return netip.AddrFrom4(info.Addr), int(info.Ifindex), true
		}
		offset += cmsgAlign(length)
	}
	return netip.Addr{}, 0, false
}

func cmsgAlign(length int) int {
	alignment := int(unsafe.Sizeof(uintptr(0)))
	return (length + alignment - 1) & ^(alignment - 1)
}
