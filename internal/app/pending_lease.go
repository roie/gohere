package app

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/roie/gohere/internal/router"
)

var pendingReservationRenewInterval = 10 * time.Second

func startPendingReservationLease(ctx context.Context, cancelStartup context.CancelFunc, client routeLifecycleClient, runID string, refs []router.RouteRef) func() error {
	if len(refs) == 0 {
		return func() error { return nil }
	}
	leaseCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	var renewalErr error
	go func() {
		defer close(done)
		ticker := time.NewTicker(pendingReservationRenewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-leaseCtx.Done():
				return
			case <-ticker.C:
				if err := client.RenewRoutes(leaseCtx, runID, refs); err != nil {
					if leaseCtx.Err() == nil {
						renewalErr = fmt.Errorf("could not renew pending route reservation: %w", err)
						cancelStartup()
					}
					return
				}
			}
		}
	}()
	stop := sync.OnceFunc(func() {
		cancel()
		<-done
	})
	return func() error {
		stop()
		return renewalErr
	}
}
