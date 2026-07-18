package lanmdns

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"
)

func testInterface() Interface {
	return Interface{Index: 7, Name: "test", Prefix: netip.MustParsePrefix("192.168.1.42/24")}
}

type fakeTransport struct {
	readCh    chan packet
	writeCh   chan outboundPacket
	closed    chan struct{}
	closeOnce sync.Once
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{
		readCh:  make(chan packet),
		writeCh: make(chan outboundPacket),
		closed:  make(chan struct{}),
	}
}

func (t *fakeTransport) ReadPacket(ctx context.Context) (packet, error) {
	select {
	case pkt := <-t.readCh:
		return pkt, nil
	case <-t.closed:
		return packet{}, errors.New("transport closed")
	case <-ctx.Done():
		return packet{}, ctx.Err()
	}
}

func (t *fakeTransport) WritePacket(ctx context.Context, pkt outboundPacket) error {
	select {
	case t.writeCh <- pkt:
		return nil
	case <-t.closed:
		return errors.New("transport closed")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (t *fakeTransport) Close() error {
	t.closeOnce.Do(func() { close(t.closed) })
	return nil
}

type immediateCoordinator struct{}

func (immediateCoordinator) Prepare(context.Context, Change) error { return nil }

func TestNewRejectsInvalidInterfaceBeforeStartingTransport(t *testing.T) {
	called := false
	_, err := New(context.Background(), Interface{}, immediateCoordinator{}, withTransportFactory(func(Interface) (transport, error) {
		called = true
		return newFakeTransport(), nil
	}))
	if err == nil {
		t.Fatal("New() error = nil")
	}
	if called {
		t.Fatal("transport factory called for invalid interface")
	}
}

func TestRegisterReturnsStableDistinctHandlesForSharedClaim(t *testing.T) {
	responder, err := New(context.Background(), testInterface(), immediateCoordinator{}, withTransport(newFakeTransport()), withImmediateRegistration())
	if err != nil {
		t.Fatal(err)
	}
	defer responder.Close()

	first, err := responder.Register(context.Background(), "shop.local")
	if err != nil {
		t.Fatal(err)
	}
	second, err := responder.Register(context.Background(), "shop.local.")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID() == second.ID() {
		t.Fatal("shared registrations have the same ID")
	}
	if first.RequestedHostname() != "shop.local." || second.RequestedHostname() != "shop.local." {
		t.Fatalf("requested names = %q, %q", first.RequestedHostname(), second.RequestedHostname())
	}
	if first.CurrentHostname() != "shop.local." || second.CurrentHostname() != "shop.local." {
		t.Fatalf("current names = %q, %q", first.CurrentHostname(), second.CurrentHostname())
	}
}

func TestClosingOneSharedHandleKeepsOtherUsable(t *testing.T) {
	responder, err := New(context.Background(), testInterface(), immediateCoordinator{}, withTransport(newFakeTransport()), withImmediateRegistration())
	if err != nil {
		t.Fatal(err)
	}
	defer responder.Close()
	first, _ := responder.Register(context.Background(), "shop.local")
	second, _ := responder.Register(context.Background(), "shop.local")
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if second.CurrentHostname() != "shop.local." {
		t.Fatalf("second current hostname = %q", second.CurrentHostname())
	}
	if err := second.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestBlockedReadPumpDoesNotBlockRegistrationOrClose(t *testing.T) {
	transport := newFakeTransport()
	responder, err := New(context.Background(), testInterface(), immediateCoordinator{}, withTransport(transport), withImmediateRegistration())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	registration, err := responder.Register(ctx, "shop.local")
	if err != nil {
		t.Fatal(err)
	}
	if err := registration.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := responder.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestResponderCloseIsIdempotent(t *testing.T) {
	responder, err := New(context.Background(), testInterface(), immediateCoordinator{}, withTransport(newFakeTransport()), withImmediateRegistration())
	if err != nil {
		t.Fatal(err)
	}
	if err := responder.Close(); err != nil {
		t.Fatal(err)
	}
	if err := responder.Close(); err != nil {
		t.Fatal(err)
	}
}
