//go:build linux || darwin

package lanmdns

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/net/ipv4"
)

func TestNativeUnixResponderAnswersLegacyQuery(t *testing.T) {
	spec := unixNativeInterface(t)
	responder, err := New(t.Context(), spec, immediateCoordinator{})
	if err != nil {
		t.Fatal(err)
	}
	defer responder.Close()

	registration, err := responder.Register(t.Context(), "gohere-native-test.local")
	if err != nil {
		t.Fatal(err)
	}
	defer registration.Close(context.Background())

	iface, err := net.InterfaceByIndex(spec.Index)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IP(spec.Prefix.Addr().AsSlice()), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	peerPacket := ipv4.NewPacketConn(peer)
	if err := peerPacket.SetMulticastInterface(iface); err != nil {
		t.Fatal(err)
	}
	if err := peerPacket.SetMulticastTTL(255); err != nil {
		t.Fatal(err)
	}

	query := new(dns.Msg)
	query.SetQuestion(registration.CurrentHostname(), dns.TypeA)
	query.Id = 9371
	payload, err := query.Pack()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := peer.WriteToUDP(payload, &net.UDPAddr{IP: net.IP(mdnsIPv4AddrPort.Addr().AsSlice()), Port: 5353}); err != nil {
		t.Fatal(err)
	}
	if err := peer.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 1500)
	n, _, err := peer.ReadFromUDP(buffer)
	if err != nil {
		t.Fatal(err)
	}
	response := unpackTestMessage(t, buffer[:n])
	if response.Id != query.Id || len(response.Answer) == 0 {
		t.Fatalf("legacy response = %#v", response)
	}
	a, ok := response.Answer[0].(*dns.A)
	if !ok || a.A.String() != spec.Prefix.Addr().String() {
		t.Fatalf("answer = %#v", response.Answer[0])
	}
}
