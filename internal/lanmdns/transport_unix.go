//go:build linux || darwin

package lanmdns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

const unixIOPollInterval = 250 * time.Millisecond

type unixTransport struct {
	spec      Interface
	conn      *net.UDPConn
	packet    *ipv4.PacketConn
	iface     *net.Interface
	closeOnce sync.Once
	closeErr  error
}

type packetMetadata struct {
	Destination    netip.AddrPort
	InterfaceIndex int
}

func newPlatformTransport(spec Interface) (transport, error) {
	if err := validateInterface(spec); err != nil {
		return nil, err
	}
	iface, err := net.InterfaceByIndex(spec.Index)
	if err != nil {
		return nil, fmt.Errorf("resolve LAN mDNS interface: %w", err)
	}
	listenConfig := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
		var controlErr error
		if err := raw.Control(func(fd uintptr) {
			if err := unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
				controlErr = err
				return
			}
			if err := unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
				controlErr = err
			}
		}); err != nil {
			return err
		}
		return controlErr
	}}
	listener, err := listenConfig.ListenPacket(context.TODO(), "udp4", "0.0.0.0:5353")
	if err != nil {
		return nil, fmt.Errorf("listen for LAN mDNS: %w", err)
	}
	conn, ok := listener.(*net.UDPConn)
	if !ok {
		closeErr := listener.Close()
		return nil, errors.Join(fmt.Errorf("LAN mDNS listener is %T, want *net.UDPConn", listener), closeErr)
	}
	packetConn := ipv4.NewPacketConn(conn)
	cleanup := func(cause error) (transport, error) {
		return nil, errors.Join(cause, conn.Close())
	}
	if err := packetConn.SetControlMessage(ipv4.FlagDst|ipv4.FlagInterface, true); err != nil {
		return cleanup(fmt.Errorf("enable LAN mDNS packet metadata: %w", err))
	}
	if err := packetConn.SetMulticastInterface(iface); err != nil {
		return cleanup(fmt.Errorf("select LAN mDNS multicast interface: %w", err))
	}
	if err := packetConn.SetMulticastTTL(255); err != nil {
		return cleanup(fmt.Errorf("set LAN mDNS multicast TTL: %w", err))
	}
	if err := packetConn.SetTTL(255); err != nil {
		return cleanup(fmt.Errorf("set LAN mDNS unicast TTL: %w", err))
	}
	if err := packetConn.JoinGroup(iface, &net.UDPAddr{IP: net.IP(mdnsIPv4AddrPort.Addr().AsSlice())}); err != nil {
		return cleanup(fmt.Errorf("join LAN mDNS multicast group: %w", err))
	}
	return &unixTransport{spec: spec, conn: conn, packet: packetConn, iface: iface}, nil
}

func (t *unixTransport) ReadPacket(ctx context.Context) (packet, error) {
	if err := ctx.Err(); err != nil {
		return packet{}, err
	}
	payload := make([]byte, 9000)
	for {
		if err := t.conn.SetReadDeadline(unixDeadline(ctx)); err != nil {
			return packet{}, err
		}
		n, control, source, err := t.packet.ReadFrom(payload)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return packet{}, ctxErr
			}
			if errors.Is(err, os.ErrDeadlineExceeded) {
				continue
			}
			return packet{}, err
		}
		metadata, ok := packetMetadataFromControl(control)
		if !ok || !acceptsInterface(t.spec, metadata.InterfaceIndex) {
			continue
		}
		udpSource, ok := source.(*net.UDPAddr)
		if !ok {
			continue
		}
		sourceAddr, ok := netip.AddrFromSlice(udpSource.IP)
		if !ok || udpSource.Port < 0 || udpSource.Port > 65535 {
			continue
		}
		return packet{
			Payload:        append([]byte(nil), payload[:n]...),
			Source:         netip.AddrPortFrom(sourceAddr.Unmap(), uint16(udpSource.Port)),
			Destination:    metadata.Destination,
			InterfaceIndex: metadata.InterfaceIndex,
		}, nil
	}
}

func (t *unixTransport) WritePacket(ctx context.Context, outbound outboundPacket) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !outbound.Destination.IsValid() || !outbound.Destination.Addr().Is4() {
		return fmt.Errorf("LAN mDNS destination must be IPv4")
	}
	if err := t.conn.SetWriteDeadline(unixDeadline(ctx)); err != nil {
		return err
	}
	control := &ipv4.ControlMessage{IfIndex: t.spec.Index, Src: net.IP(t.spec.Prefix.Addr().AsSlice())}
	destination := &net.UDPAddr{IP: net.IP(outbound.Destination.Addr().AsSlice()), Port: int(outbound.Destination.Port())}
	_, err := t.packet.WriteTo(outbound.Payload, control, destination)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return err
	}
	return nil
}

func (t *unixTransport) Close() error {
	t.closeOnce.Do(func() { t.closeErr = t.packet.Close() })
	return t.closeErr
}

func packetMetadataFromControl(control *ipv4.ControlMessage) (packetMetadata, bool) {
	if control == nil || control.IfIndex <= 0 {
		return packetMetadata{}, false
	}
	destination, ok := netip.AddrFromSlice(control.Dst)
	if !ok {
		return packetMetadata{}, false
	}
	destination = destination.Unmap()
	if !destination.Is4() {
		return packetMetadata{}, false
	}
	return packetMetadata{
		Destination:    netip.AddrPortFrom(destination, 5353),
		InterfaceIndex: control.IfIndex,
	}, true
}

func unixDeadline(ctx context.Context) time.Time {
	deadline := time.Now().Add(unixIOPollInterval)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		return contextDeadline
	}
	return deadline
}
