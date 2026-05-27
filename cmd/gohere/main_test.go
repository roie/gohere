package main

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestWaitForRouterStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	routerDone := make(chan struct{})
	done := make(chan struct{})

	go func() {
		waitForRouter(ctx, routerDone)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waitForRouter did not return after context cancellation")
	}
}

func TestWaitForRouterStopsOnRouterDone(t *testing.T) {
	ctx := context.Background()
	routerDone := make(chan struct{})
	done := make(chan struct{})

	go func() {
		waitForRouter(ctx, routerDone)
		close(done)
	}()

	close(routerDone)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waitForRouter did not return after router shutdown")
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

func TestPrintUsageIndentsExamples(t *testing.T) {
	var out bytes.Buffer
	printUsage(&out, "")

	if !bytes.Contains(out.Bytes(), []byte("\n  gohere --target 5173 -- npm run dev\n")) {
		t.Fatalf("usage output = %q", out.String())
	}
}

func TestPrintUsageDescribesFlags(t *testing.T) {
	var out bytes.Buffer
	printUsage(&out, "")

	for _, want := range []string{
		"--open",
		"open the URL in your browser",
		"--as NAME",
		"use NAME.localhost for this run",
		"--verbose",
		"show target, command, and service details",
		"--target PORT",
		"use an existing local port",
		"--port-flag FLAG",
		"override the dev server port flag",
	} {
		if !bytes.Contains(out.Bytes(), []byte(want)) {
			t.Fatalf("usage output missing %q:\n%s", want, out.String())
		}
	}
}

func TestPrintUsageDoctorTopicIsSpecific(t *testing.T) {
	var out bytes.Buffer
	printUsage(&out, "doctor")

	for _, want := range []string{
		"Usage:\n  gohere doctor\n",
		"Checks the gohere service",
	} {
		if !bytes.Contains(out.Bytes(), []byte(want)) {
			t.Fatalf("doctor usage missing %q:\n%s", want, out.String())
		}
	}
	if bytes.Contains(out.Bytes(), []byte("gohere pages/about.html")) {
		t.Fatalf("doctor usage should not include generic examples:\n%s", out.String())
	}
}

func TestPrintUsageListTopicIsSpecific(t *testing.T) {
	var out bytes.Buffer
	printUsage(&out, "list")

	for _, want := range []string{
		"Usage:\n  gohere list [--verbose]\n",
		"Shows active .localhost routes",
	} {
		if !bytes.Contains(out.Bytes(), []byte(want)) {
			t.Fatalf("list usage missing %q:\n%s", want, out.String())
		}
	}
	if bytes.Contains(out.Bytes(), []byte("gohere pages/about.html")) {
		t.Fatalf("list usage should not include generic examples:\n%s", out.String())
	}
}

func TestPrintUsageStopTopicIsSpecific(t *testing.T) {
	var out bytes.Buffer
	printUsage(&out, "stop")

	for _, want := range []string{
		"Usage:\n  gohere stop [route|project|--all]\n",
		"Stops routes by current context",
	} {
		if !bytes.Contains(out.Bytes(), []byte(want)) {
			t.Fatalf("stop usage missing %q:\n%s", want, out.String())
		}
	}
	if bytes.Contains(out.Bytes(), []byte("gohere pages/about.html")) {
		t.Fatalf("stop usage should not include generic examples:\n%s", out.String())
	}
}
