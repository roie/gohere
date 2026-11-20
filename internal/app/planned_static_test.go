package app

import (
	"context"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/roie/gohere/internal/admin"
	"github.com/roie/gohere/internal/cli"
	"github.com/roie/gohere/internal/router"
)

func TestPlannedStaticStopsAfterRouteRemoval(t *testing.T) {
	original := routeLeaseInterval
	routeLeaseInterval = 10 * time.Millisecond
	defer func() { routeLeaseInterval = original }()
	item := plannedProcessItem(t, t.TempDir(), 0)
	item.plan.Static = true
	item.plan.StaticBindHost = "127.0.0.1"
	if err := os.WriteFile(filepath.Join(item.plan.CWD, "index.html"), []byte("planned-owned-server"), 0600); err != nil {
		t.Fatal(err)
	}
	store := router.NewMemoryStore()
	server := httptest.NewServer(router.NewServer(router.Config{Token: "test-token", Store: store}).AdminHandler())
	defer server.Close()
	client := admin.NewClient(server.URL, "test-token")
	oldClient := defaultAdminClientFunc
	defaultAdminClientFunc = func() (adminClient, error) { return client, nil }
	defer func() { defaultAdminClientFunc = oldClient }()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = runPlannedSingle(ctx, item.cmd, item.plan, client, io.Discard, io.Discard)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("static cleanup timed out")
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		routes, err := client.Routes(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(routes) == 1 && routes[0].EffectiveState() == router.RouteStateActive {
			if routes[0].PID != 0 || routes[0].ProcessIdentity != "" {
				t.Fatalf("static route claims a child process: %+v", routes[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("static activation timed out")
		}
		time.Sleep(time.Millisecond)
	}
	if !plannedServerReachable(item.plan.Port) {
		t.Fatal("static server not serving")
	}
	if err := StopWithCommand(t.Context(), cli.Command{Kind: cli.CommandStop}, item.plan.CWD, io.Discard); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		if runErr != nil {
			t.Fatal(runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("static server ignored route removal")
	}
	if plannedServerReachable(item.plan.Port) {
		t.Fatal("static listener remains after stop")
	}
}
