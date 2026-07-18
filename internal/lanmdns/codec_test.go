package lanmdns

import (
	"net/netip"
	"testing"

	"github.com/miekg/dns"
)

const testHostname = "shop.local."

var testIPv4 = netip.MustParseAddr("192.168.1.42")

func unpackTestMessage(t *testing.T, payload []byte) *dns.Msg {
	t.Helper()
	msg := new(dns.Msg)
	if err := msg.Unpack(payload); err != nil {
		t.Fatalf("Unpack() error = %v", err)
	}
	return msg
}

func TestProbeUsesANYINAndAuthorityA(t *testing.T) {
	payload, err := probeMessage(testHostname, testIPv4)
	if err != nil {
		t.Fatal(err)
	}
	msg := unpackTestMessage(t, payload)
	if len(msg.Question) != 1 {
		t.Fatalf("questions = %d, want 1", len(msg.Question))
	}
	question := msg.Question[0]
	if question.Name != testHostname || question.Qtype != dns.TypeANY || question.Qclass != dns.ClassINET {
		t.Fatalf("question = %+v", question)
	}
	if len(msg.Ns) != 1 {
		t.Fatalf("authority records = %d, want 1", len(msg.Ns))
	}
	a, ok := msg.Ns[0].(*dns.A)
	if !ok || a.Hdr.Name != testHostname || a.Hdr.Class != dns.ClassINET || a.A.String() != testIPv4.String() {
		t.Fatalf("authority = %#v", msg.Ns[0])
	}
}

func TestAnnouncementContainsCacheFlushAAndNSECWithTTL120(t *testing.T) {
	payload, err := announcementMessage(testHostname, testIPv4, recordTTL)
	if err != nil {
		t.Fatal(err)
	}
	msg := unpackTestMessage(t, payload)
	if !msg.Response || !msg.Authoritative || len(msg.Answer) != 2 {
		t.Fatalf("announcement = %#v", msg)
	}
	assertARecord(t, msg.Answer[0], recordTTL, true)
	assertNSECRecord(t, msg.Answer[1], recordTTL, true)
}

func TestAAAAQueryGetsNSECNegativeAnswer(t *testing.T) {
	query := new(dns.Msg)
	query.SetQuestion(testHostname, dns.TypeAAAA)
	payload, err := responseMessage(query, testHostname, testIPv4, false)
	if err != nil {
		t.Fatal(err)
	}
	msg := unpackTestMessage(t, payload)
	if len(msg.Answer) != 1 {
		t.Fatalf("answers = %d, want 1", len(msg.Answer))
	}
	assertNSECRecord(t, msg.Answer[0], recordTTL, true)
}

func TestANYQueryGetsAAndNSEC(t *testing.T) {
	query := new(dns.Msg)
	query.SetQuestion(testHostname, dns.TypeANY)
	payload, err := responseMessage(query, testHostname, testIPv4, false)
	if err != nil {
		t.Fatal(err)
	}
	msg := unpackTestMessage(t, payload)
	if len(msg.Answer) != 2 {
		t.Fatalf("answers = %d, want 2", len(msg.Answer))
	}
	assertARecord(t, msg.Answer[0], recordTTL, true)
	assertNSECRecord(t, msg.Answer[1], recordTTL, true)
}

func TestLegacyResponseRepeatsIDAndQuestionWithoutCacheFlushAndTTL10(t *testing.T) {
	query := new(dns.Msg)
	query.SetQuestion(testHostname, dns.TypeA)
	query.Id = 4242
	payload, err := responseMessage(query, testHostname, testIPv4, true)
	if err != nil {
		t.Fatal(err)
	}
	msg := unpackTestMessage(t, payload)
	if msg.Id != query.Id || len(msg.Question) != 1 || msg.Question[0] != query.Question[0] {
		t.Fatalf("legacy header/question = %#v", msg)
	}
	if len(msg.Answer) != 1 {
		t.Fatalf("answers = %d, want 1", len(msg.Answer))
	}
	assertARecord(t, msg.Answer[0], legacyTTL, false)
}

func TestGoodbyeUsesTTLZero(t *testing.T) {
	payload, err := goodbyeMessage(testHostname, testIPv4)
	if err != nil {
		t.Fatal(err)
	}
	msg := unpackTestMessage(t, payload)
	if len(msg.Answer) != 2 {
		t.Fatalf("answers = %d, want 2", len(msg.Answer))
	}
	assertARecord(t, msg.Answer[0], 0, true)
	assertNSECRecord(t, msg.Answer[1], 0, true)
}

func TestDecodeRejectsMalformedAndNonZeroOpcode(t *testing.T) {
	if _, err := decodePacket([]byte{0, 1, 2}); err == nil {
		t.Fatal("decodePacket(malformed) error = nil")
	}
	msg := new(dns.Msg)
	msg.Opcode = dns.OpcodeUpdate
	payload, err := msg.Pack()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodePacket(payload); err == nil {
		t.Fatal("decodePacket(non-query opcode) error = nil")
	}
}

func assertARecord(t *testing.T, rr dns.RR, ttl uint32, flush bool) {
	t.Helper()
	a, ok := rr.(*dns.A)
	if !ok {
		t.Fatalf("record = %T, want *dns.A", rr)
	}
	if a.Hdr.Name != testHostname || a.Hdr.Ttl != ttl || a.A.String() != testIPv4.String() {
		t.Fatalf("A = %#v", a)
	}
	if got := a.Hdr.Class&cacheFlushBit != 0; got != flush {
		t.Fatalf("A cache flush = %v, want %v", got, flush)
	}
}

func assertNSECRecord(t *testing.T, rr dns.RR, ttl uint32, flush bool) {
	t.Helper()
	nsec, ok := rr.(*dns.NSEC)
	if !ok {
		t.Fatalf("record = %T, want *dns.NSEC", rr)
	}
	if nsec.Hdr.Name != testHostname || nsec.NextDomain != testHostname || nsec.Hdr.Ttl != ttl {
		t.Fatalf("NSEC = %#v", nsec)
	}
	if len(nsec.TypeBitMap) != 1 || nsec.TypeBitMap[0] != dns.TypeA {
		t.Fatalf("NSEC bitmap = %v, want [A]", nsec.TypeBitMap)
	}
	if got := nsec.Hdr.Class&cacheFlushBit != 0; got != flush {
		t.Fatalf("NSEC cache flush = %v, want %v", got, flush)
	}
}
