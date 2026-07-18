package lanmdns

import (
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestQueryResponseDelay(t *testing.T) {
	random := func(max time.Duration) time.Duration { return max }
	unique := new(dns.Msg)
	unique.SetQuestion(testHostname, dns.TypeA)
	if got := queryResponseDelay(unique, random); got != 0 {
		t.Fatalf("unique delay = %v", got)
	}
	multiple := new(dns.Msg)
	multiple.Question = []dns.Question{
		{Name: testHostname, Qtype: dns.TypeA, Qclass: dns.ClassINET},
		{Name: testHostname, Qtype: dns.TypeAAAA, Qclass: dns.ClassINET},
	}
	if got := queryResponseDelay(multiple, random); got != 120*time.Millisecond {
		t.Fatalf("multiple delay = %v, want 120ms", got)
	}
	truncated := new(dns.Msg)
	truncated.SetQuestion(testHostname, dns.TypeA)
	truncated.Truncated = true
	if got := queryResponseDelay(truncated, random); got != 500*time.Millisecond {
		t.Fatalf("truncated delay = %v, want 500ms", got)
	}
}

func TestMulticastScheduleHonorsRecordRateLimit(t *testing.T) {
	now := time.Unix(100, 0)
	if got := multicastDue(now, now.Add(-500*time.Millisecond), false); got != now.Add(500*time.Millisecond) {
		t.Fatalf("ordinary due = %v", got)
	}
	if got := multicastDue(now, now.Add(-100*time.Millisecond), true); got != now.Add(150*time.Millisecond) {
		t.Fatalf("defense due = %v", got)
	}
}
