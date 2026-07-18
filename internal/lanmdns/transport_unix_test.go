//go:build linux || darwin

package lanmdns

import (
	"context"
	"net"
	"net/netip"
	"os"
	"strconv"
	"testing"
	"time"

	"golang.org/x/net/ipv4"
)

func unixNativeInterface(t *testing.T) Interface {
	t.Helper()
	if os.Getenv("GOHERE_NATIVE_MDNS_TEST") != "1" {
		t.Skip("set GOHERE_NATIVE_MDNS_TEST=1 on a dedicated native runner")
	}
	index, err := strconv.Atoi(os.Getenv("LAN_MDNS_INTERFACE_INDEX"))
	if err != nil || index <= 0 {
		t.Fatal("LAN_MDNS_INTERFACE_INDEX must be a positive integer")
	}
	prefix, err := netip.ParsePrefix(os.Getenv("LAN_MDNS_PREFIX"))
	if err != nil {
		t.Fatalf("LAN_MDNS_PREFIX: %v", err)
	}
	iface, err := net.InterfaceByIndex(index)
	if err != nil {
		t.Fatal(err)
	}
	return Interface{Index: index, Name: iface.Name, Prefix: prefix}
}

func TestUnixPacketFromControlMessage(t *testing.T) {
	control := &ipv4.ControlMessage{Dst: net.ParseIP("224.0.0.251"), IfIndex: 7}
	got, ok := packetMetadataFromControl(control)
	if !ok {
		t.Fatal("packetMetadataFromControl() ok = false")
	}
	if got.Destination != mdnsIPv4AddrPort || got.InterfaceIndex != 7 {
		t.Fatalf("metadata = %+v", got)
	}
}

func TestUnixTransportSelectedInterfaceLoopback(t *testing.T) {
	spec := unixNativeInterface(t)
	tr, err := newPlatformTransport(spec)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	payload := []byte("gohere-unix-mdns-spike")
	if err := tr.WritePacket(ctx, outboundPacket{Payload: payload, Destination: mdnsIPv4AddrPort}); err != nil {
		t.Fatal(err)
	}
	packet, err := tr.ReadPacket(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(packet.Payload) != string(payload) || packet.InterfaceIndex != spec.Index || packet.Destination != mdnsIPv4AddrPort {
		t.Fatalf("packet = %+v", packet)
	}
}

func TestUnixTransportCloseUnblocksRead(t *testing.T) {
	tr, err := newPlatformTransport(unixNativeInterface(t))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := tr.ReadPacket(context.Background())
		done <- err
	}()
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not unblock ReadPacket")
	}
}
