package lanmdns

import (
	"bytes"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"
)

const (
	conflictWindow       = 10 * time.Second
	conflictFloodBackoff = 5 * time.Second
	probingFailureLimit  = time.Minute
)

func nextConflictHostname(hostname string) (string, error) {
	hostname, err := canonicalHostname(hostname)
	if err != nil {
		return "", err
	}
	label := strings.TrimSuffix(hostname, ".local.")
	base := label
	number := 2
	if dash := strings.LastIndexByte(label, '-'); dash > 0 && dash < len(label)-1 {
		if parsed, parseErr := strconv.Atoi(label[dash+1:]); parseErr == nil && parsed >= 2 {
			base = label[:dash]
			number = parsed + 1
		}
	}
	suffix := "-" + strconv.Itoa(number)
	if len(base)+len(suffix) > 63 {
		base = base[:63-len(suffix)]
	}
	if base == "" {
		return "", fmt.Errorf("mDNS hostname cannot be suffixed")
	}
	return base + suffix + ".local.", nil
}

func compareProbeIPv4(ours, theirs netip.Addr) int {
	if !ours.Is4() || !theirs.Is4() {
		return 0
	}
	oursBytes := ours.As4()
	theirsBytes := theirs.As4()
	return bytes.Compare(oursBytes[:], theirsBytes[:])
}

func conflictBackoff(now time.Time, conflicts []time.Time) time.Duration {
	cutoff := now.Add(-conflictWindow)
	count := 0
	for _, conflict := range conflicts {
		if !conflict.Before(cutoff) && !conflict.After(now) {
			count++
		}
	}
	if count >= 15 {
		return conflictFloodBackoff
	}
	return 0
}

func responseClaimsName(msg *dns.Msg, hostname string) bool {
	if msg == nil || !msg.Response {
		return false
	}
	for _, section := range [][]dns.RR{msg.Answer, msg.Ns, msg.Extra} {
		for _, record := range section {
			if strings.EqualFold(record.Header().Name, hostname) && record.Header().Rrtype != dns.TypeNSEC {
				return true
			}
		}
	}
	return false
}

func probeOpponentIPv4(msg *dns.Msg, hostname string) (netip.Addr, bool) {
	if msg == nil || msg.Response {
		return netip.Addr{}, false
	}
	asked := false
	for _, question := range msg.Question {
		if strings.EqualFold(question.Name, hostname) && question.Qtype == dns.TypeANY {
			asked = true
			break
		}
	}
	if !asked {
		return netip.Addr{}, false
	}
	for _, record := range msg.Ns {
		a, ok := record.(*dns.A)
		if !ok || !strings.EqualFold(a.Hdr.Name, hostname) {
			continue
		}
		addr, ok := netip.AddrFromSlice(a.A.To4())
		return addr, ok
	}
	return netip.Addr{}, false
}
