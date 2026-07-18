package lanmdns

import (
	"bytes"
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestNextConflictHostnameIsDeterministic(t *testing.T) {
	tests := map[string]string{
		"shop.local.":   "shop-2.local.",
		"shop-2.local.": "shop-3.local.",
		"shop-9.local.": "shop-10.local.",
	}
	for input, want := range tests {
		got, err := nextConflictHostname(input)
		if err != nil {
			t.Fatalf("nextConflictHostname(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("nextConflictHostname(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSimultaneousProbeTieBreakUsesLexicographicallyLaterRDATA(t *testing.T) {
	lower := netip.MustParseAddr("192.168.1.41")
	higher := netip.MustParseAddr("192.168.1.42")
	if compareProbeIPv4(higher, lower) <= 0 {
		t.Fatal("higher IPv4 should win")
	}
	if compareProbeIPv4(lower, higher) >= 0 {
		t.Fatal("lower IPv4 should lose")
	}
	if compareProbeIPv4(higher, higher) != 0 {
		t.Fatal("identical IPv4 should tie")
	}
}

func TestFifteenConflictsInTenSecondsRequireFiveSecondBackoff(t *testing.T) {
	now := time.Unix(100, 0)
	conflicts := make([]time.Time, 15)
	for i := range conflicts {
		conflicts[i] = now.Add(-time.Duration(i) * 500 * time.Millisecond)
	}
	if got := conflictBackoff(now, conflicts); got != 5*time.Second {
		t.Fatalf("conflictBackoff() = %v, want 5s", got)
	}
	if got := conflictBackoff(now, conflicts[:14]); got != 0 {
		t.Fatalf("conflictBackoff(14) = %v, want 0", got)
	}
}

func TestPositiveRecordAtProbingNameCausesSuffixAndReprobe(t *testing.T) {
	clock := newManualClock()
	transport := newFakeTransport()
	transport.writeCh = make(chan outboundPacket, 8)
	responder, err := New(t.Context(), testInterface(), immediateCoordinator{},
		withTransport(transport), withClock(clock), withProbeDelay(func(time.Duration) time.Duration { return 0 }))
	if err != nil {
		t.Fatal(err)
	}
	defer responder.Close()

	go responder.Register(t.Context(), "shop.local")
	assertProbeWrite(t, transport.writeCh)

	conflict := &dns.Msg{MsgHdr: dns.MsgHdr{Response: true, Authoritative: true}, Answer: []dns.RR{
		&dns.TXT{Hdr: dns.RR_Header{Name: "shop.local.", Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 120}, Txt: []string{"owned"}},
	}}
	payload, err := conflict.Pack()
	if err != nil {
		t.Fatal(err)
	}
	transport.readCh <- packet{Payload: payload, Source: netip.MustParseAddrPort("192.168.1.50:5353"), Destination: mdnsIPv4AddrPort, InterfaceIndex: testInterface().Index}

	select {
	case write := <-transport.writeCh:
		msg := unpackTestMessage(t, write.Payload)
		if got := msg.Question[0].Name; got != "shop-2.local." {
			t.Fatalf("reprobe name = %q, want shop-2.local.", got)
		}
		if a := msg.Ns[0].(*dns.A); !bytes.Equal(a.A.To4(), testIPv4.AsSlice()) {
			t.Fatalf("reprobe A = %s", a.A)
		}
	case <-time.After(time.Second):
		t.Fatal("conflict did not trigger reprobe")
	}
}
