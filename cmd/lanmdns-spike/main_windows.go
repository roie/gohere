//go:build windows

package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/netip"
	"os"
	"time"

	"github.com/roie/gohere/internal/lanmdns"
)

func main() {
	interfaceIndex := flag.Int("interface-index", 0, "Windows interface index")
	prefixText := flag.String("prefix", "", "selected private IPv4 prefix")
	flag.Parse()

	prefix, err := netip.ParsePrefix(*prefixText)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --prefix: %v\n", err)
		os.Exit(2)
	}
	iface, err := net.InterfaceByIndex(*interfaceIndex)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --interface-index: %v\n", err)
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := lanmdns.RunWindowsTransportSpike(ctx, lanmdns.Interface{
		Index:  iface.Index,
		Name:   iface.Name,
		Prefix: prefix,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "Windows LAN mDNS transport spike failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Windows LAN mDNS transport spike passed")
}
