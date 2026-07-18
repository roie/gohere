package lanmdns

import (
	"net/netip"
	"strings"
	"time"

	"github.com/miekg/dns"
)

func conflictingAResponse(msg *dns.Msg, hostname string, addr netip.Addr) bool {
	if msg == nil || !msg.Response {
		return false
	}
	for _, section := range [][]dns.RR{msg.Answer, msg.Ns, msg.Extra} {
		for _, record := range section {
			a, ok := record.(*dns.A)
			if !ok || !strings.EqualFold(a.Hdr.Name, hostname) {
				continue
			}
			claimed, ok := netip.AddrFromSlice(a.A.To4())
			if ok && claimed != addr {
				return true
			}
		}
	}
	return false
}

func queryResponseDelay(msg *dns.Msg, random func(time.Duration) time.Duration) time.Duration {
	if msg.Truncated {
		return 400*time.Millisecond + random(100*time.Millisecond)
	}
	if len(msg.Question) > 1 {
		return 20*time.Millisecond + random(100*time.Millisecond)
	}
	return 0
}

func multicastDue(now, lastMulticast time.Time, probeDefense bool) time.Time {
	minimum := time.Second
	if probeDefense {
		minimum = 250 * time.Millisecond
	}
	due := lastMulticast.Add(minimum)
	if due.Before(now) {
		return now
	}
	return due
}

func knownAnswerSuppresses(msg *dns.Msg, hostname string, addr netip.Addr) bool {
	for _, record := range msg.Answer {
		a, ok := record.(*dns.A)
		if !ok || !strings.EqualFold(a.Hdr.Name, hostname) || a.Hdr.Ttl < recordTTL/2 {
			continue
		}
		known, ok := netip.AddrFromSlice(a.A.To4())
		if ok && known == addr {
			return true
		}
	}
	return false
}

func responseDestination(inbound packet, question dns.Question, lastMulticast, now time.Time, prefix netip.Prefix) (netip.AddrPort, bool) {
	if inbound.Source.Port() != 5353 {
		if !prefix.Contains(inbound.Source.Addr()) {
			return netip.AddrPort{}, false
		}
		return inbound.Source, true
	}
	qu := question.Qclass&cacheFlushBit != 0
	if qu && prefix.Contains(inbound.Source.Addr()) && now.Sub(lastMulticast) < time.Duration(recordTTL/4)*time.Second {
		return inbound.Source, true
	}
	return mdnsIPv4AddrPort, true
}
