package app

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/roie/gohere/internal/cli"
	"github.com/roie/gohere/internal/runner"
)

func TestPreparePortLaunch(t *testing.T) {
	dir := tempProject(t, map[string]string{"package.json": `{"scripts":{"dev":"vite"}}`})
	cmd, err := cli.Parse([]string{"gohere", ":5173"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PrepareRun(cmd, dir)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Port != 5173 || !plan.FixedPort || !plan.ManagedPort ||
		!sameStrings(plan.Command, []string{"npm", "run", "dev", "--", "--host", "127.0.0.1", "--port", "5173", "--strictPort"}) {
		t.Fatalf("plan = %#v", plan)
	}
}

func TestRunConnectDoesNotStartServer(t *testing.T) {
	oldClient, oldRunner, oldWSL := defaultAdminClientFunc, startRunnerFunc, detectWSLFunc
	t.Cleanup(func() {
		defaultAdminClientFunc, startRunnerFunc, detectWSLFunc = oldClient, oldRunner, oldWSL
	})
	client := &recordingAdminClient{}
	defaultAdminClientFunc = func() (adminClient, error) { return client, nil }
	detectWSLFunc = func() bool { return false }
	startRunnerFunc = func(context.Context, runner.Config) (*runner.Result, error) {
		t.Error("connecting must not start a child")
		return nil, runner.ErrProcessFinished
	}

	// The .git directory stops the package.json search at the test folder.
	dir := tempProject(t, map[string]string{".git/HEAD": ""})
	cmd, err := cli.Parse([]string{"gohere", "+5173"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var stdout, stderr strings.Builder
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cmd, dir, &stdout, &stderr) }()
	client.waitForUpsert(t)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if client.route.Target != "http://127.0.0.1:5173" || client.route.PID != 0 || client.route.Mode != "connect" {
		t.Fatalf("route = %#v", client.route)
	}
	if client.deleteRefCalls != 1 || len(client.routes) != 0 {
		t.Fatalf("route was not cleaned up: deletes=%d routes=%#v", client.deleteRefCalls, client.routes)
	}
	if !strings.Contains(stdout.String(), "http://"+filepath.Base(dir)+".localhost") || stderr.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestRunConnectRejectsUnreachableTarget(t *testing.T) {
	oldClient, oldWSL := defaultAdminClientFunc, detectWSLFunc
	t.Cleanup(func() { defaultAdminClientFunc, detectWSLFunc = oldClient, oldWSL })
	client := &recordingAdminClient{probeReachable: map[string]bool{}}
	defaultAdminClientFunc = func() (adminClient, error) { return client, nil }
	detectWSLFunc = func() bool { return false }
	cmd, err := cli.Parse([]string{"gohere", "+5173"})
	if err != nil {
		t.Fatal(err)
	}
	err = Run(t.Context(), cmd, t.TempDir(), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no server is reachable on port 5173") || strings.Contains(err.Error(), "0.0.0.0") {
		t.Fatalf("error = %v", err)
	}
	if client.activateCalls != 0 || len(client.routes) != 0 {
		t.Fatalf("unreachable target left a route: %#v", client.routes)
	}
}

func TestRunConnectFromWSLRoutesThroughWindows(t *testing.T) {
	client := &recordingAdminClient{}
	t.Cleanup(stubBridgeDetection(t, bridgeStub{
		isWSL: true, token: "windows-token", wslIP: "172.20.10.2", reachable: true,
		windowsBinary: true, admin: client, localAdmin: fakeAdminClient{},
	}))
	cmd, err := cli.Parse([]string{"gohere", "+5173"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cmd, t.TempDir(), io.Discard, io.Discard) }()
	client.waitForUpsert(t)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if client.route.Target != "http://127.0.0.1:5173" || client.route.OwnerEnv != "wsl" || client.route.Mode != "connect" || client.route.PID != 0 {
		t.Fatalf("route = %#v", client.route)
	}
	if client.deleteRefCalls != 1 || len(client.routes) != 0 {
		t.Fatalf("route was not cleaned up: deletes=%d routes=%#v", client.deleteRefCalls, client.routes)
	}
}

func TestRunConnectFromWSLExplainsLoopbackOnlyServer(t *testing.T) {
	oldTimeout, oldInterval := wslTargetProbeTimeout, wslTargetProbeInterval
	wslTargetProbeTimeout, wslTargetProbeInterval = 10*time.Millisecond, time.Millisecond
	t.Cleanup(func() { wslTargetProbeTimeout, wslTargetProbeInterval = oldTimeout, oldInterval })
	client := &recordingAdminClient{probeReachable: map[string]bool{}}
	t.Cleanup(stubBridgeDetection(t, bridgeStub{
		isWSL: true, token: "windows-token", wslIP: "172.20.10.2", windowsBinary: true,
		probeReachable: map[string]bool{"172.20.10.2": true},
		admin:          client, localAdmin: fakeAdminClient{},
	}))
	cmd, err := cli.Parse([]string{"gohere", "+5173"})
	if err != nil {
		t.Fatal(err)
	}
	err = Run(t.Context(), cmd, t.TempDir(), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no server is reachable on port 5173") || !strings.Contains(err.Error(), "172.20.10.2") || !strings.Contains(err.Error(), "0.0.0.0") {
		t.Fatalf("error = %v", err)
	}
	if client.activateCalls != 0 || len(client.routes) != 0 {
		t.Fatalf("unreachable target left a route: %#v", client.routes)
	}
}
