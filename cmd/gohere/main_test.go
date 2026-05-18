package main

import (
	"bytes"
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

func TestPrintVersion(t *testing.T) {
	oldVersion := version
	defer func() {
		version = oldVersion
	}()

	version = "0.1.0"
	var out bytes.Buffer
	printVersion(&out)

	if out.String() != "gohere 0.1.0\n" {
		t.Fatalf("version output = %q", out.String())
	}
}
