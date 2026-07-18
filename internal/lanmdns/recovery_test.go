package lanmdns

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestRuntimeConflictLossRenamesAndUpdatesStableHandle(t *testing.T) {
	responder, clock, transport, registration := activeTestResponder(t)
	defer responder.Close()

	sendConflictingA(t, transport, testHostname, "192.168.1.99")
	assertProbeName(t, transport.writeCh, testHostname)
	sendConflictingA(t, transport, testHostname, "192.168.1.99")
	assertProbeName(t, transport.writeCh, "shop-2.local.")
	clock.AdvanceToTimer(t, 250*time.Millisecond)
	assertProbeName(t, transport.writeCh, "shop-2.local.")
	clock.AdvanceToTimer(t, 250*time.Millisecond)
	assertProbeName(t, transport.writeCh, "shop-2.local.")
	clock.AdvanceToTimer(t, 250*time.Millisecond)
	assertAnnouncementWrite(t, transport.writeCh)
	if got := registration.CurrentHostname(); got != "shop-2.local." {
		t.Fatalf("CurrentHostname() = %q, want shop-2.local.", got)
	}
}

func TestCanceledRegistrationDoesNotAnnounce(t *testing.T) {
	clock := newManualClock()
	transport := newFakeTransport()
	transport.writeCh = make(chan outboundPacket, 8)
	responder, err := New(t.Context(), testInterface(), immediateCoordinator{},
		withTransport(transport), withClock(clock), withProbeDelay(func(time.Duration) time.Duration { return 0 }))
	if err != nil {
		t.Fatal(err)
	}
	defer responder.Close()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := responder.Register(ctx, testHostname)
		result <- err
	}()
	assertProbeWrite(t, transport.writeCh)
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("Register() error = nil after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("Register did not observe cancellation")
	}
	time.Sleep(20 * time.Millisecond)
	clock.Advance(250 * time.Millisecond)
	select {
	case write := <-transport.writeCh:
		msg := unpackTestMessage(t, write.Payload)
		t.Fatalf("canceled registration continued with response=%v", msg.Response)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestConcurrentRegisterAndRemoveIsRaceClean(t *testing.T) {
	responder, err := New(t.Context(), testInterface(), immediateCoordinator{},
		withTransport(newFakeTransport()), withImmediateRegistration())
	if err != nil {
		t.Fatal(err)
	}
	defer responder.Close()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			registration, err := responder.Register(t.Context(), testHostname)
			if err == nil {
				_ = registration.Close(t.Context())
			}
		}()
	}
	wg.Wait()
}

func sendConflictingA(t *testing.T, transport *fakeTransport, hostname, ip string) {
	t.Helper()
	message := &dns.Msg{MsgHdr: dns.MsgHdr{Response: true, Authoritative: true}, Answer: []dns.RR{
		&dns.A{Hdr: dns.RR_Header{Name: hostname, Rrtype: dns.TypeA, Class: dns.ClassINET | cacheFlushBit, Ttl: recordTTL}, A: net.ParseIP(ip)},
	}}
	sendTestQuery(t, transport, message, netip.MustParseAddrPort(ip+":5353"), mdnsIPv4AddrPort)
}

func assertProbeName(t *testing.T, writes <-chan outboundPacket, want string) {
	t.Helper()
	select {
	case write := <-writes:
		message := unpackTestMessage(t, write.Payload)
		if len(message.Question) != 1 || message.Question[0].Name != want {
			t.Fatalf("probe question = %#v, want %s", message.Question, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("probe for %s was not written", want)
	}
}
