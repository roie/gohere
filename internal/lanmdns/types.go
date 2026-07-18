package lanmdns

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
)

var mdnsIPv4AddrPort = netip.MustParseAddrPort("224.0.0.251:5353")

// Interface identifies the single IPv4 interface owned by a responder.
type Interface struct {
	Index  int
	Name   string
	Prefix netip.Prefix
}

func validateInterface(spec Interface) error {
	if spec.Index <= 0 {
		return fmt.Errorf("LAN mDNS interface index must be positive")
	}
	if strings.TrimSpace(spec.Name) == "" {
		return fmt.Errorf("LAN mDNS interface name is required")
	}
	if !spec.Prefix.IsValid() || !spec.Prefix.Addr().Is4() {
		return fmt.Errorf("LAN mDNS interface prefix must be IPv4")
	}
	addr := spec.Prefix.Addr()
	if !addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() {
		return fmt.Errorf("LAN mDNS interface address must be private IPv4")
	}
	return nil
}

type packet struct {
	Payload        []byte
	Source         netip.AddrPort
	Destination    netip.AddrPort
	InterfaceIndex int
}

type outboundPacket struct {
	Payload     []byte
	Destination netip.AddrPort
}

type transport interface {
	ReadPacket(context.Context) (packet, error)
	WritePacket(context.Context, outboundPacket) error
	Close() error
}

func acceptsInterface(spec Interface, index int) bool {
	return index > 0 && index == spec.Index
}
