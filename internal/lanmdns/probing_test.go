package lanmdns

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
)

type manualTimer struct {
	clock    *manualClock
	channel  chan time.Time
	deadline time.Time
	active   bool
}

type manualClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*manualTimer
}

func newManualClock() *manualClock {
	return &manualClock{now: time.Unix(1_700_000_000, 0)}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) NewTimer(duration time.Duration) actorTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &manualTimer{clock: c, channel: make(chan time.Time, 1), deadline: c.now.Add(duration), active: true}
	c.timers = append(c.timers, timer)
	return timer
}

func (c *manualClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	now := c.now
	var due []*manualTimer
	for _, timer := range c.timers {
		if timer.active && !timer.deadline.After(now) {
			timer.active = false
			due = append(due, timer)
		}
	}
	c.mu.Unlock()
	for _, timer := range due {
		timer.channel <- now
	}
}

func (t *manualTimer) C() <-chan time.Time { return t.channel }
func (t *manualTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	wasActive := t.active
	t.active = false
	return wasActive
}
func (t *manualTimer) Reset(duration time.Duration) bool {
	t.clock.mu.Lock()
	wasActive := t.active
	now := t.clock.now
	t.deadline = now.Add(duration)
	t.active = duration > 0
	select {
	case <-t.channel:
	default:
	}
	t.clock.mu.Unlock()
	if duration <= 0 {
		t.channel <- now
	}
	return wasActive
}

func TestProbeSequenceIncludesFinalObservationWindow(t *testing.T) {
	clock := newManualClock()
	transport := newFakeTransport()
	transport.writeCh = make(chan outboundPacket, 8)
	responder, err := New(context.Background(), testInterface(), immediateCoordinator{},
		withTransport(transport), withClock(clock), withProbeDelay(func(time.Duration) time.Duration { return 0 }))
	if err != nil {
		t.Fatal(err)
	}
	defer responder.Close()

	result := make(chan Registration, 1)
	errs := make(chan error, 1)
	go func() {
		registration, err := responder.Register(context.Background(), "shop.local")
		if err != nil {
			errs <- err
			return
		}
		result <- registration
	}()

	assertProbeWrite(t, transport.writeCh)
	clock.Advance(250 * time.Millisecond)
	assertProbeWrite(t, transport.writeCh)
	clock.Advance(250 * time.Millisecond)
	assertProbeWrite(t, transport.writeCh)

	select {
	case <-result:
		t.Fatal("Register returned before final 250ms observation")
	case err := <-errs:
		t.Fatal(err)
	default:
	}

	clock.Advance(250 * time.Millisecond)
	assertAnnouncementWrite(t, transport.writeCh)
	clock.Advance(time.Second)
	assertAnnouncementWrite(t, transport.writeCh)

	select {
	case registration := <-result:
		if registration.CurrentHostname() != "shop.local." {
			t.Fatalf("CurrentHostname() = %q", registration.CurrentHostname())
		}
	case err := <-errs:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("Register did not complete after second announcement")
	}
}

func assertProbeWrite(t *testing.T, writes <-chan outboundPacket) {
	t.Helper()
	select {
	case write := <-writes:
		msg := unpackTestMessage(t, write.Payload)
		if len(msg.Question) != 1 || msg.Question[0].Qtype != dns.TypeANY || msg.Response {
			t.Fatalf("write is not a probe: %#v", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("probe was not written")
	}
}

func assertAnnouncementWrite(t *testing.T, writes <-chan outboundPacket) {
	t.Helper()
	select {
	case write := <-writes:
		msg := unpackTestMessage(t, write.Payload)
		if !msg.Response || !msg.Authoritative {
			t.Fatalf("write is not an announcement: %#v", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("announcement was not written")
	}
}
