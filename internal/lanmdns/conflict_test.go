package lanmdns

import (
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestConflictingResponseStopsAnswersAndReprobesSameName(t *testing.T) {
	responder, clock, transport, registration := activeTestResponder(t)
	defer responder.Close()

	conflict := &dns.Msg{MsgHdr: dns.MsgHdr{Response: true, Authoritative: true}, Answer: []dns.RR{
		&dns.A{Hdr: dns.RR_Header{Name: testHostname, Rrtype: dns.TypeA, Class: dns.ClassINET | cacheFlushBit, Ttl: recordTTL}, A: net.ParseIP("192.168.1.99")},
	}}
	sendTestQuery(t, transport, conflict, netip.MustParseAddrPort("192.168.1.99:5353"), mdnsIPv4AddrPort)

	assertProbeWrite(t, transport.writeCh)
	if registration.CurrentHostname() != testHostname {
		t.Fatalf("CurrentHostname() = %q during reprobe", registration.CurrentHostname())
	}

	query := new(dns.Msg)
	query.SetQuestion(testHostname, dns.TypeA)
	sendTestQuery(t, transport, query, netip.MustParseAddrPort("192.168.1.50:5353"), mdnsIPv4AddrPort)
	select {
	case response := <-transport.writeCh:
		t.Fatalf("reprobing name answered with %d bytes", len(response.Payload))
	case <-time.After(50 * time.Millisecond):
	}

	clock.Advance(250 * time.Millisecond)
	assertProbeWrite(t, transport.writeCh)
	clock.Advance(250 * time.Millisecond)
	assertProbeWrite(t, transport.writeCh)
	clock.Advance(250 * time.Millisecond)
	assertAnnouncementWrite(t, transport.writeCh)
}

func TestActiveOwnerDefendsProbe(t *testing.T) {
	responder, clock, transport, _ := activeTestResponder(t)
	defer responder.Close()
	clock.Advance(250 * time.Millisecond)

	probe := new(dns.Msg)
	probe.Question = []dns.Question{{Name: testHostname, Qtype: dns.TypeANY, Qclass: dns.ClassINET}}
	probe.Ns = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: testHostname, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: recordTTL}, A: net.ParseIP("192.168.1.99")}}
	sendTestQuery(t, transport, probe, netip.MustParseAddrPort("192.168.1.99:5353"), mdnsIPv4AddrPort)
	response := unpackTestMessage(t, readTestResponse(t, transport.writeCh).Payload)
	if !response.Response || !response.Authoritative {
		t.Fatalf("defense = %#v", response)
	}
}

func TestIdenticalAResponseDoesNotTriggerReprobe(t *testing.T) {
	responder, _, transport, _ := activeTestResponder(t)
	defer responder.Close()
	identical := &dns.Msg{MsgHdr: dns.MsgHdr{Response: true, Authoritative: true}, Answer: []dns.RR{
		&dns.A{Hdr: dns.RR_Header{Name: testHostname, Rrtype: dns.TypeA, Class: dns.ClassINET | cacheFlushBit, Ttl: recordTTL}, A: net.ParseIP(testIPv4.String())},
	}}
	sendTestQuery(t, transport, identical, netip.MustParseAddrPort("192.168.1.50:5353"), mdnsIPv4AddrPort)
	select {
	case write := <-transport.writeCh:
		t.Fatalf("identical response triggered %d-byte write", len(write.Payload))
	case <-time.After(50 * time.Millisecond):
	}
}
