//go:build windows

package lanmdns

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"strconv"
	"testing"
	"time"
)

func windowsNativeInterface(t *testing.T) Interface {
	t.Helper()
	if os.Getenv("GOHERE_NATIVE_MDNS_TEST") != "1" {
		t.Skip("set GOHERE_NATIVE_MDNS_TEST=1 on the dedicated Windows runner")
	}
	index, err := strconv.Atoi(os.Getenv("LAN_MDNS_INTERFACE_INDEX"))
	if err != nil || index <= 0 {
		t.Fatal("LAN_MDNS_INTERFACE_INDEX must be a positive integer")
	}
	prefix, err := netip.ParsePrefix(os.Getenv("LAN_MDNS_PREFIX"))
	if err != nil {
		t.Fatalf("LAN_MDNS_PREFIX: %v", err)
	}
	return Interface{Index: index, Name: "native-test", Prefix: prefix}
}

func TestWindowsTransportCloseUnblocksRead(t *testing.T) {
	tr, err := newPlatformTransport(windowsNativeInterface(t))
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
	case err := <-done:
		if err == nil {
			t.Fatal("ReadPacket() error = nil after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not unblock ReadPacket")
	}
}

func TestWindowsTransportRejectsOtherInterfaceMetadata(t *testing.T) {
	spec := windowsNativeInterface(t)
	if acceptsInterface(spec, 0) || acceptsInterface(spec, spec.Index+1) {
		t.Fatal("unexpected interface accepted")
	}
	if !acceptsInterface(spec, spec.Index) {
		t.Fatal("selected interface rejected")
	}
}

func TestWindowsTransportUsesTTL255(t *testing.T) {
	tr, err := newPlatformTransport(windowsNativeInterface(t))
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	settings, ok := tr.(interface {
		socketTTLs() (multicast, unicast int, err error)
	})
	if !ok {
		t.Fatal("transport does not expose native spike TTL verification")
	}
	multicast, unicast, err := settings.socketTTLs()
	if err != nil {
		t.Fatal(err)
	}
	if multicast != 255 || unicast != 255 {
		t.Fatalf("TTLs = multicast %d, unicast %d; want 255, 255", multicast, unicast)
	}
}

func TestWindowsTransportSelectedInterfaceLoopback(t *testing.T) {
	tr, err := newPlatformTransport(windowsNativeInterface(t))
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	payload := []byte("gohere-windows-mdns-spike")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := tr.WritePacket(ctx, outboundPacket{Payload: payload, Destination: mdnsIPv4AddrPort}); err != nil {
		t.Fatal(err)
	}
	pkt, err := tr.ReadPacket(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(pkt.Payload) != string(payload) {
		t.Fatalf("payload = %q", pkt.Payload)
	}
	if pkt.InterfaceIndex != windowsNativeInterface(t).Index {
		t.Fatalf("interface = %d", pkt.InterfaceIndex)
	}
	if pkt.Destination != mdnsIPv4AddrPort {
		t.Fatalf("destination = %s", pkt.Destination)
	}
}

func TestRunWindowsTransportSpike(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := RunWindowsTransportSpike(ctx, windowsNativeInterface(t)); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsTransportReadHonorsCancellation(t *testing.T) {
	tr, err := newPlatformTransport(windowsNativeInterface(t))
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = tr.ReadPacket(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadPacket() error = %v, want context.Canceled", err)
	}
}
