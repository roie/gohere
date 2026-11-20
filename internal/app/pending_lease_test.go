package app

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/roie/gohere/internal/router"
)

type pendingLeaseClient struct {
	routeLifecycleClient
	renew func(context.Context, string, []router.RouteRef) error
}

func (c pendingLeaseClient) RenewRoutes(ctx context.Context, runID string, refs []router.RouteRef) error {
	return c.renew(ctx, runID, refs)
}

func TestPendingReservationLeaseFailure(t *testing.T) {
	original := pendingReservationRenewInterval
	if original != 10*time.Second {
		t.Fatalf("pending renewal interval = %v", original)
	}
	pendingReservationRenewInterval = time.Millisecond
	defer func() { pendingReservationRenewInterval = original }()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	failure := errors.New("reservation ownership lost")
	refs := []router.RouteRef{{ID: "first", Generation: 2}, {ID: "second", Generation: 4}}
	client := pendingLeaseClient{renew: func(_ context.Context, runID string, got []router.RouteRef) error {
		if runID != "run" || len(got) != 2 || got[0] != refs[0] || got[1] != refs[1] {
			t.Errorf("renewed wrong reservation: %s %+v", runID, got)
		}
		return failure
	}}
	stop := startPendingReservationLease(ctx, cancel, client, "run", refs)
	defer stop()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("renewal failure did not cancel startup")
	}
	for range 2 {
		if !errors.Is(stop(), failure) {
			t.Fatal("renewal error was not preserved")
		}
	}
}

func TestPendingReservationLeaseHandoffJoinsRenewal(t *testing.T) {
	originalPending, originalActive := pendingReservationRenewInterval, routeLeaseInterval
	pendingReservationRenewInterval, routeLeaseInterval = time.Millisecond, 5*time.Millisecond
	defer func() { pendingReservationRenewInterval, routeLeaseInterval = originalPending, originalActive }()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered, release, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
	releaseRenewal := sync.OnceFunc(func() { close(release) })
	client := pendingLeaseClient{renew: func(ctx context.Context, _ string, _ []router.RouteRef) error {
		close(entered)
		<-ctx.Done()
		<-release
		close(exited)
		return errors.New("transport interrupted")
	}}
	refs := []router.RouteRef{{ID: "owned", Generation: 3}}
	stop := startPendingReservationLease(ctx, cancel, client, "run", refs)
	defer stop()
	defer releaseRenewal()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("pending loop did not renew")
	}
	var activeCalls atomic.Int32
	activeRenewed := make(chan struct{}, 1)
	active := pendingLeaseClient{renew: func(context.Context, string, []router.RouteRef) error {
		activeCalls.Add(1)
		select {
		case activeRenewed <- struct{}{}:
		default:
		}
		return nil
	}}
	stopActive := startReservationLease(ctx, active, "run", refs, io.Discard)
	defer stopActive()
	joined := make(chan error, 1)
	go func() { joined <- stop() }()
	select {
	case <-joined:
		t.Error("handoff returned before pending renewal exited")
	case <-time.After(20 * time.Millisecond):
	}
	select {
	case <-activeRenewed:
	case <-time.After(time.Second):
		t.Fatal("active renewal did not cover the pending handoff")
	}
	releaseRenewal()
	select {
	case err := <-joined:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending loop did not join")
	}
	select {
	case <-exited:
	default:
		t.Fatal("pending request leaked")
	}
	if ctx.Err() != nil {
		t.Fatal("handoff canceled the running server context")
	}
	select {
	case <-activeRenewed:
	case <-time.After(time.Second):
		t.Fatal("active renewal did not take over")
	}
	stopActive()
	calls := activeCalls.Load()
	time.Sleep(15 * time.Millisecond)
	if activeCalls.Load() != calls {
		t.Fatal("active renewal leaked after cleanup")
	}
}

func TestPendingReservationLeaseSkipsReusedRoutes(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := pendingLeaseClient{renew: func(context.Context, string, []router.RouteRef) error {
		t.Error("reused-only launch renewed a pending reservation")
		return nil
	}}
	if err := startPendingReservationLease(ctx, cancel, client, "run", nil)(); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil {
		t.Fatal("reused-only launch was canceled")
	}
}
