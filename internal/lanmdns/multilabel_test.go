package lanmdns

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestCanonicalHostnameAcceptsMultiLabelLocalNames(t *testing.T) {
	valid := map[string]string{
		"app.local":                             "app.local.",
		"web.repo.local":                        "web.repo.local.",
		" API.Repo.Local. ":                     "api.repo.local.",
		"a.b.c.d.local":                         "a.b.c.d.local.",
		strings.Repeat("a", 63) + ".repo.local": strings.Repeat("a", 63) + ".repo.local.",
	}
	for input, want := range valid {
		got, err := canonicalHostname(input)
		if err != nil || got != want {
			t.Fatalf("canonicalHostname(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{
		"", "local", ".local", "..local", "web..local", ".web.local",
		"web.repo.localhost", "web.local.com",
		strings.Repeat("a", 64) + ".repo.local",
		strings.Repeat(strings.Repeat("a", 63)+".", 4) + "local",
	} {
		if got, err := canonicalHostname(input); err == nil {
			t.Fatalf("canonicalHostname(%q) = %q, want error", input, got)
		}
	}
}

func TestNextConflictHostnameSuffixesFirstLabelOfMultiLabelName(t *testing.T) {
	long := strings.Repeat("a", 63)
	tests := map[string]string{
		"web.repo.local.":            "web-2.repo.local.",
		"web-2.repo.local.":          "web-3.repo.local.",
		"api.repo-2.local.":          "api-2.repo-2.local.",
		long + ".repo.local.":        long[:61] + "-2.repo.local.",
		long[:61] + "-9.repo.local.": long[:60] + "-10.repo.local.",
	}
	for input, want := range tests {
		got, err := nextConflictHostname(input)
		if err != nil || got != want {
			t.Fatalf("nextConflictHostname(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
}

func TestMultiLabelNamePublishesAnswersAndRenamesOnConflict(t *testing.T) {
	const hostname = "web.repo.local."
	responder, clock, transport, registration := activeTestResponderFor(t, "Web.Repo.Local")
	defer responder.Close()
	if got := registration.CurrentHostname(); got != hostname {
		t.Fatalf("CurrentHostname() = %q, want %q", got, hostname)
	}

	clock.Advance(time.Second)
	query := new(dns.Msg)
	query.SetQuestion("WEB.repo.LOCAL.", dns.TypeA)
	sendTestQuery(t, transport, query, netip.MustParseAddrPort("192.168.1.50:5353"), mdnsIPv4AddrPort)
	msg := unpackTestMessage(t, readTestResponse(t, transport.writeCh).Payload)
	if len(msg.Answer) != 1 {
		t.Fatalf("answers = %d, want 1", len(msg.Answer))
	}
	if a, ok := msg.Answer[0].(*dns.A); !ok || a.Hdr.Name != hostname || a.A.String() != testIPv4.String() {
		t.Fatalf("answer = %#v", msg.Answer[0])
	}

	sendConflictingA(t, transport, hostname, "192.168.1.99")
	assertProbeName(t, transport.writeCh, hostname)
	sendConflictingA(t, transport, hostname, "192.168.1.99")
	assertProbeName(t, transport.writeCh, "web-2.repo.local.")
	clock.AdvanceToTimer(t, 250*time.Millisecond)
	assertProbeName(t, transport.writeCh, "web-2.repo.local.")
	clock.AdvanceToTimer(t, 250*time.Millisecond)
	assertProbeName(t, transport.writeCh, "web-2.repo.local.")
	clock.AdvanceToTimer(t, 250*time.Millisecond)
	assertAnnouncementWrite(t, transport.writeCh)
	if got := registration.CurrentHostname(); got != "web-2.repo.local." {
		t.Fatalf("CurrentHostname() = %q, want web-2.repo.local.", got)
	}
}

func TestRegisterRejectsInvalidLocalName(t *testing.T) {
	responder, _, _, _ := activeTestResponder(t)
	defer responder.Close()
	for _, hostname := range []string{"web..local", "web.repo.localhost", strings.Repeat("a", 64) + ".repo.local"} {
		if _, err := responder.Register(t.Context(), hostname); err == nil {
			t.Fatalf("Register(%q) error = nil", hostname)
		}
	}
}
