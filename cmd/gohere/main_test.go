package main

import (
	"context"
	"testing"
	"time"
)

func TestWaitForRouterStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		waitForRouter(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waitForRouter did not return after context cancellation")
	}
}
