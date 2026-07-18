package lanmdns

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func activeTestResponder(t *testing.T) (*Responder, *manualClock, *fakeTransport, Registration) {
	t.Helper()
	clock := newManualClock()
	transport := newFakeTransport()
	transport.writeCh = make(chan outboundPacket, 16)
	responder, err := New(t.Context(), testInterface(), immediateCoordinator{},
		withTransport(transport), withClock(clock), withProbeDelay(func(time.Duration) time.Duration { return 0 }))
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan Registration, 1)
	go func() {
		registration, _ := responder.Register(context.Background(), "shop.local")
		result <- registration
	}()
	assertProbeWrite(t, transport.writeCh)
	clock.Advance(250 * time.Millisecond)
	assertProbeWrite(t, transport.writeCh)
	clock.Advance(250 * time.Millisecond)
	assertProbeWrite(t, transport.writeCh)
	clock.Advance(250 * time.Millisecond)
	assertAnnouncementWrite(t, transport.writeCh)
	clock.Advance(time.Second)
	assertAnnouncementWrite(t, transport.writeCh)
	select {
	case registration := <-result:
		return responder, clock, transport, registration
	case <-time.After(time.Second):
		t.Fatal("registration did not activate")
		return nil, nil, nil, nil
	}
}

func TestActiveNameAnswersAQuery(t *testing.T) {
	responder, clock, transport, _ := activeTestResponder(t)
	defer responder.Close()
	clock.Advance(time.Second)
	query := new(dns.Msg)
	query.SetQuestion(testHostname, dns.TypeA)
	sendTestQuery(t, transport, query, netip.MustParseAddrPort("192.168.1.50:5353"), mdnsIPv4AddrPort)
	response := readTestResponse(t, transport.writeCh)
	if response.Destination != mdnsIPv4AddrPort {
		t.Fatalf("destination = %s", response.Destination)
	}
	msg := unpackTestMessage(t, response.Payload)
	if len(msg.Answer) != 1 {
		t.Fatalf("answers = %d, want 1", len(msg.Answer))
	}
	assertARecord(t, msg.Answer[0], recordTTL, true)
}

func TestAAAAQueryReturnsNSEC(t *testing.T) {
	responder, clock, transport, _ := activeTestResponder(t)
	defer responder.Close()
	clock.Advance(time.Second)
	query := new(dns.Msg)
	query.SetQuestion(testHostname, dns.TypeAAAA)
	sendTestQuery(t, transport, query, netip.MustParseAddrPort("192.168.1.50:5353"), mdnsIPv4AddrPort)
	msg := unpackTestMessage(t, readTestResponse(t, transport.writeCh).Payload)
	assertNSECRecord(t, msg.Answer[0], recordTTL, true)
}

func TestLegacyQueryGetsConventionalUnicastResponse(t *testing.T) {
	responder, _, transport, _ := activeTestResponder(t)
	defer responder.Close()
	query := new(dns.Msg)
	query.SetQuestion(testHostname, dns.TypeA)
	query.Id = 8123
	source := netip.MustParseAddrPort("192.168.1.50:49152")
	sendTestQuery(t, transport, query, source, mdnsIPv4AddrPort)
	response := readTestResponse(t, transport.writeCh)
	if response.Destination != source {
		t.Fatalf("destination = %s, want %s", response.Destination, source)
	}
	msg := unpackTestMessage(t, response.Payload)
	if msg.Id != query.Id || len(msg.Question) != 1 {
		t.Fatalf("legacy response = %#v", msg)
	}
	assertARecord(t, msg.Answer[0], legacyTTL, false)
}

func TestKnownAnswerAtHalfTTLIsSuppressed(t *testing.T) {
	responder, _, transport, _ := activeTestResponder(t)
	defer responder.Close()
	query := new(dns.Msg)
	query.SetQuestion(testHostname, dns.TypeA)
	a, err := aRecord(testHostname, testIPv4, recordTTL/2, true)
	if err != nil {
		t.Fatal(err)
	}
	query.Answer = []dns.RR{a}
	sendTestQuery(t, transport, query, netip.MustParseAddrPort("192.168.1.50:5353"), mdnsIPv4AddrPort)
	select {
	case response := <-transport.writeCh:
		t.Fatalf("suppressed query wrote %d bytes", len(response.Payload))
	case <-time.After(50 * time.Millisecond):
	}
}

func TestMulticastResponseWaitsForOneSecondRecordInterval(t *testing.T) {
	responder, clock, transport, _ := activeTestResponder(t)
	defer responder.Close()
	query := new(dns.Msg)
	query.SetQuestion(testHostname, dns.TypeA)
	sendTestQuery(t, transport, query, netip.MustParseAddrPort("192.168.1.50:5353"), mdnsIPv4AddrPort)
	select {
	case <-transport.writeCh:
		t.Fatal("response ignored one-second multicast interval")
	case <-time.After(50 * time.Millisecond):
	}
	clock.Advance(time.Second)
	msg := unpackTestMessage(t, readTestResponse(t, transport.writeCh).Payload)
	assertARecord(t, msg.Answer[0], recordTTL, true)
}

func TestRecentMulticastQUUsesUnicast(t *testing.T) {
	responder, _, transport, _ := activeTestResponder(t)
	defer responder.Close()
	query := new(dns.Msg)
	query.SetQuestion(testHostname, dns.TypeA)
	query.Question[0].Qclass |= cacheFlushBit
	source := netip.MustParseAddrPort("192.168.1.50:5353")
	sendTestQuery(t, transport, query, source, mdnsIPv4AddrPort)
	if got := readTestResponse(t, transport.writeCh).Destination; got != source {
		t.Fatalf("QU destination = %s, want %s", got, source)
	}
}

func sendTestQuery(t *testing.T, transport *fakeTransport, query *dns.Msg, source, destination netip.AddrPort) {
	t.Helper()
	payload, err := query.Pack()
	if err != nil {
		t.Fatal(err)
	}
	transport.readCh <- packet{Payload: payload, Source: source, Destination: destination, InterfaceIndex: testInterface().Index}
}

func readTestResponse(t *testing.T, writes <-chan outboundPacket) outboundPacket {
	t.Helper()
	select {
	case response := <-writes:
		return response
	case <-time.After(time.Second):
		t.Fatal("response was not written")
		return outboundPacket{}
	}
}
