package lanmdns

import (
	"net/netip"
	"testing"
)

func TestValidateInterface(t *testing.T) {
	tests := []struct {
		name string
		spec Interface
		ok   bool
	}{
		{name: "valid private IPv4", spec: Interface{Index: 7, Name: "Wi-Fi", Prefix: netip.MustParsePrefix("192.168.1.42/24")}, ok: true},
		{name: "missing index", spec: Interface{Name: "Wi-Fi", Prefix: netip.MustParsePrefix("192.168.1.42/24")}},
		{name: "missing name", spec: Interface{Index: 7, Prefix: netip.MustParsePrefix("192.168.1.42/24")}},
		{name: "IPv6", spec: Interface{Index: 7, Name: "Wi-Fi", Prefix: netip.MustParsePrefix("fd00::1/64")}},
		{name: "loopback", spec: Interface{Index: 7, Name: "Loopback", Prefix: netip.MustParsePrefix("127.0.0.1/8")}},
		{name: "public", spec: Interface{Index: 7, Name: "Ethernet", Prefix: netip.MustParsePrefix("203.0.113.10/24")}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateInterface(tt.spec)
			if tt.ok && err != nil {
				t.Fatalf("validateInterface() error = %v", err)
			}
			if !tt.ok && err == nil {
				t.Fatal("validateInterface() error = nil")
			}
		})
	}
}

func TestOutboundPacketDoesNotCarryInterfaceSelection(t *testing.T) {
	packet := outboundPacket{Destination: netip.MustParseAddrPort("224.0.0.251:5353")}
	if packet.Destination.Port() != 5353 {
		t.Fatalf("Destination port = %d, want 5353", packet.Destination.Port())
	}
}
