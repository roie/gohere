package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/roie/gohere/internal/cli"
	"github.com/roie/gohere/internal/runner"
)

func TestPlannedWSLOwnershipBoundaryFailures(t *testing.T) {
	for _, failure := range []string{"foreign-linux-listener", "linux-lookup-unavailable", "windows-transport-failure"} {
		t.Run(failure, func(t *testing.T) {
			port, err := runner.ChooseFreePort()
			if err != nil {
				t.Fatal(err)
			}
			mode := "serve"
			client := &recordingAdminClient{}
			switch failure {
			case "foreign-linux-listener":
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
				defer server.Close()
				port = server.Listener.Addr().(*net.TCPAddr).Port
				mode = "advertise"
			case "linux-lookup-unavailable":
				start := startRunnerFunc
				startRunnerFunc = func(context.Context, runner.Config) (*runner.Result, error) {
					return nil, errors.New("simulated Linux ownership lookup failure")
				}
				defer func() { startRunnerFunc = start }()
			case "windows-transport-failure":
				client.probeErr = errors.New("simulated Windows transport failure")
			}
			plan := ownershipRunPlan(t, mode, port)
			err = runPlannedSingle(t.Context(), cli.Command{Kind: cli.CommandRun, Script: "dev", HTTP: true}, plan, client, io.Discard, io.Discard)
			if err == nil {
				t.Fatal("boundary failure was accepted")
			}
			if client.activateCalls != 0 || client.reserveCalls != 1 || client.releaseCalls != 1 || len(client.routes) != 0 {
				t.Fatalf("activate=%d reserve=%d release=%d remaining=%d", client.activateCalls, client.reserveCalls, client.releaseCalls, len(client.routes))
			}
			if failure != "windows-transport-failure" && client.probeCalls != 0 {
				t.Fatal("Windows reachability was consulted before Linux ownership was established")
			}
			if failure == "windows-transport-failure" && !strings.Contains(err.Error(), "simulated Windows transport failure") {
				t.Fatalf("expected transport failure after local ownership, got %v", err)
			}
		})
	}
}

func TestPlannedGroupRejectsUnrelatedListenerBeforeActivation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()
	foreignPort := server.Listener.Addr().(*net.TCPAddr).Port
	ownedPort, err := runner.ChooseFreePort()
	if err != nil {
		t.Fatal(err)
	}
	owned := ownershipRunPlan(t, "serve", ownedPort)
	foreign := ownershipRunPlan(t, "advertise", foreignPort)
	foreign.Name, foreign.Host = "api", "api.localhost"
	client := &recordingAdminClient{}
	err = runPlannedGroup(t.Context(), false, []multiRunItem{
		{cmd: cli.Command{Kind: cli.CommandRun, Script: "dev:web", HTTP: true}, plan: owned},
		{cmd: cli.Command{Kind: cli.CommandRun, Script: "dev:api", HTTP: true}, plan: foreign},
	}, client, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "belongs to another process") {
		t.Fatalf("group error = %v", err)
	}
	if client.activateCalls != 0 || client.releaseCalls != 1 || len(client.routes) != 0 {
		t.Fatalf("activate=%d release=%d remaining=%d", client.activateCalls, client.releaseCalls, len(client.routes))
	}
}

func ownershipRunPlan(t *testing.T, mode string, port int) RunPlan {
	t.Helper()
	return RunPlan{
		Command: []string{os.Args[0], "-test.run=^TestOwnershipBoundaryHelper$"},
		Env:     append(os.Environ(), "GOHERE_BOUNDARY_HELPER="+mode, "PORT="+strconv.Itoa(port)),
		Port:    port, Host: "web.localhost", Name: "web", CWD: t.TempDir(),
		URLScheme: "http", RouteTargetHost: "172.20.10.2", ListenHost: "0.0.0.0",
		OwnerEnv: "wsl", ManagedPort: true,
	}
}

func TestOwnershipBoundaryHelper(t *testing.T) {
	mode := os.Getenv("GOHERE_BOUNDARY_HELPER")
	if mode == "" {
		return
	}
	port := os.Getenv("PORT")
	if mode == "advertise" {
		fmt.Printf("Local: http://127.0.0.1:%s\n", port)
		time.Sleep(10 * time.Second)
		return
	}
	listener, err := net.Listen("tcp4", "0.0.0.0:"+port)
	if err != nil {
		os.Exit(1)
	}
	fmt.Printf("Local: http://127.0.0.1:%s\n", port)
	http.Serve(listener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
}
