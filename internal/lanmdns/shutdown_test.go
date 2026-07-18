package lanmdns

import (
	"context"
	"testing"
	"time"
)

func TestFinalRegistrationCloseSendsGoodbye(t *testing.T) {
	responder, _, transport, registration := activeTestResponder(t)
	defer responder.Close()
	if err := registration.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	message := unpackTestMessage(t, readTestResponse(t, transport.writeCh).Payload)
	if len(message.Answer) != 2 || message.Answer[0].Header().Ttl != 0 || message.Answer[1].Header().Ttl != 0 {
		t.Fatalf("goodbye = %#v", message.Answer)
	}
}

func TestResponderCloseSendsGoodbyeBeforeTransportClose(t *testing.T) {
	responder, _, transport, _ := activeTestResponder(t)
	if err := responder.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case write := <-transport.writeCh:
		message := unpackTestMessage(t, write.Payload)
		if message.Answer[0].Header().Ttl != 0 {
			t.Fatalf("goodbye TTL = %d", message.Answer[0].Header().Ttl)
		}
	default:
		t.Fatal("Close did not write a goodbye")
	}
}

func TestResponderCloseRemainsBoundedWhenWriterIsBlocked(t *testing.T) {
	clock := newManualClock()
	transport := newFakeTransport()
	responder, err := New(context.Background(), testInterface(), immediateCoordinator{},
		withTransport(transport), withClock(clock), withImmediateRegistration())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := responder.Register(context.Background(), "shop.local"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- responder.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked on writer")
	}
}
