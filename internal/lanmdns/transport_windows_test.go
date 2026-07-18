//go:build windows

package lanmdns

import (
	"context"
	"errors"
	"net"
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
	return windowsInterfaceFromEnv(t, "LAN_MDNS_INTERFACE_INDEX", "LAN_MDNS_PREFIX")
}

func windowsInterfaceFromEnv(t *testing.T, indexVariable, prefixVariable string) Interface {
	t.Helper()
	index, err := strconv.Atoi(os.Getenv(indexVariable))
	if err != nil || index <= 0 {
		t.Fatalf("%s must be a positive integer", indexVariable)
	}
	prefix, err := netip.ParsePrefix(os.Getenv(prefixVariable))
	if err != nil {
		t.Fatalf("%s: %v", prefixVariable, err)
	}
	return Interface{Index: index, Name: "native-test", Prefix: prefix}
}

func TestWindowsTransportHostedSmoke(t *testing.T) {
	if os.Getenv("GOHERE_WINDOWS_MDNS_SMOKE") != "1" {
		t.Skip("set GOHERE_WINDOWS_MDNS_SMOKE=1 on a hosted Windows runner")
	}
	connection, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.ParseIP("1.1.1.1"), Port: 53})
	if err != nil {
		t.Fatal(err)
	}
	localIP := connection.LocalAddr().(*net.UDPAddr).IP
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagMulticast == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			prefix, err := netip.ParsePrefix(address.String())
			if err != nil || !prefix.Addr().Is4() || prefix.Addr().String() != localIP.String() {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err = RunWindowsTransportSpike(ctx, Interface{Index: iface.Index, Name: iface.Name, Prefix: prefix})
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatalf("default-route interface for %s was not found", localIP)
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

func TestRunWindowsTransportIsolationSpike(t *testing.T) {
	selected := windowsNativeInterface(t)
	excluded := windowsInterfaceFromEnv(t, "LAN_MDNS_EXCLUDED_INTERFACE_INDEX", "LAN_MDNS_EXCLUDED_PREFIX")
	if selected.Index == excluded.Index {
		t.Fatal("selected and excluded interface indexes must differ")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := RunWindowsTransportIsolationSpike(ctx, selected, excluded); err != nil {
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
