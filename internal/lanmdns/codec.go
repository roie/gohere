package lanmdns

import (
	"fmt"
	"net"
	"net/netip"
	"strings"

	"github.com/miekg/dns"
)

const (
	recordTTL     uint32 = 120
	legacyTTL     uint32 = 10
	cacheFlushBit uint16 = 1 << 15
)

func decodePacket(payload []byte) (*dns.Msg, error) {
	msg := new(dns.Msg)
	if err := msg.Unpack(payload); err != nil {
		return nil, fmt.Errorf("decode mDNS packet: %w", err)
	}
	if msg.Opcode != dns.OpcodeQuery {
		return nil, fmt.Errorf("unsupported mDNS opcode %d", msg.Opcode)
	}
	return msg, nil
}

func probeMessage(hostname string, addr netip.Addr) ([]byte, error) {
	hostname, err := canonicalHostname(hostname)
	if err != nil {
		return nil, err
	}
	a, err := aRecord(hostname, addr, recordTTL, false)
	if err != nil {
		return nil, err
	}
	msg := &dns.Msg{
		MsgHdr: dns.MsgHdr{Id: 0},
		Question: []dns.Question{{
			Name:   hostname,
			Qtype:  dns.TypeANY,
			Qclass: dns.ClassINET,
		}},
		Ns: []dns.RR{a},
	}
	return packMessage(msg)
}

func announcementMessage(hostname string, addr netip.Addr, ttl uint32) ([]byte, error) {
	hostname, err := canonicalHostname(hostname)
	if err != nil {
		return nil, err
	}
	records, err := ownedRecords(hostname, addr, ttl, true)
	if err != nil {
		return nil, err
	}
	return packMessage(&dns.Msg{
		MsgHdr: dns.MsgHdr{Response: true, Authoritative: true},
		Answer: records,
	})
}

func goodbyeMessage(hostname string, addr netip.Addr) ([]byte, error) {
	return announcementMessage(hostname, addr, 0)
}

func responseMessage(query *dns.Msg, hostname string, addr netip.Addr, legacy bool) ([]byte, error) {
	if query == nil || len(query.Question) != 1 {
		return nil, fmt.Errorf("mDNS response requires exactly one question")
	}
	hostname, err := canonicalHostname(hostname)
	if err != nil {
		return nil, err
	}
	question := query.Question[0]
	if !strings.EqualFold(dns.Fqdn(question.Name), hostname) {
		return nil, fmt.Errorf("mDNS question name %q does not match %q", question.Name, hostname)
	}
	if question.Qclass&^cacheFlushBit != dns.ClassINET && question.Qclass != dns.ClassANY {
		return nil, fmt.Errorf("unsupported mDNS question class %d", question.Qclass)
	}

	ttl := recordTTL
	flush := true
	msg := &dns.Msg{MsgHdr: dns.MsgHdr{Response: true, Authoritative: true}}
	if legacy {
		ttl = legacyTTL
		flush = false
		msg.SetReply(query)
		msg.Authoritative = true
	}

	switch question.Qtype {
	case dns.TypeA:
		a, err := aRecord(hostname, addr, ttl, flush)
		if err != nil {
			return nil, err
		}
		msg.Answer = []dns.RR{a}
	case dns.TypeANY:
		msg.Answer, err = ownedRecords(hostname, addr, ttl, flush)
		if err != nil {
			return nil, err
		}
	default:
		msg.Answer = []dns.RR{nsecRecord(hostname, ttl, flush)}
	}
	return packMessage(msg)
}

func ownedRecords(hostname string, addr netip.Addr, ttl uint32, flush bool) ([]dns.RR, error) {
	a, err := aRecord(hostname, addr, ttl, flush)
	if err != nil {
		return nil, err
	}
	return []dns.RR{a, nsecRecord(hostname, ttl, flush)}, nil
}

func aRecord(hostname string, addr netip.Addr, ttl uint32, flush bool) (*dns.A, error) {
	if !addr.IsValid() || !addr.Is4() {
		return nil, fmt.Errorf("mDNS A record address must be IPv4")
	}
	class := uint16(dns.ClassINET)
	if flush {
		class |= cacheFlushBit
	}
	return &dns.A{
		Hdr: dns.RR_Header{Name: hostname, Rrtype: dns.TypeA, Class: class, Ttl: ttl},
		A:   net.IP(addr.AsSlice()),
	}, nil
}

func nsecRecord(hostname string, ttl uint32, flush bool) *dns.NSEC {
	class := uint16(dns.ClassINET)
	if flush {
		class |= cacheFlushBit
	}
	return &dns.NSEC{
		Hdr:        dns.RR_Header{Name: hostname, Rrtype: dns.TypeNSEC, Class: class, Ttl: ttl},
		NextDomain: hostname,
		TypeBitMap: []uint16{dns.TypeA},
	}
}

func canonicalHostname(hostname string) (string, error) {
	hostname = strings.ToLower(dns.Fqdn(strings.TrimSpace(hostname)))
	if !strings.HasSuffix(hostname, ".local.") {
		return "", fmt.Errorf("mDNS hostname must end in .local")
	}
	label := strings.TrimSuffix(hostname, ".local.")
	if label == "" || strings.Contains(label, ".") || len(label) > 63 {
		return "", fmt.Errorf("mDNS hostname must contain one label of at most 63 bytes")
	}
	if _, ok := dns.IsDomainName(hostname); !ok {
		return "", fmt.Errorf("invalid mDNS hostname %q", hostname)
	}
	return hostname, nil
}

func packMessage(msg *dns.Msg) ([]byte, error) {
	payload, err := msg.Pack()
	if err != nil {
		return nil, fmt.Errorf("encode mDNS packet: %w", err)
	}
	return payload, nil
}
