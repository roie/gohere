package app

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/roie/gohere/internal/admin"
	"github.com/roie/gohere/internal/cli"
	"github.com/roie/gohere/internal/lifecycle"
	"github.com/roie/gohere/internal/router"
	"github.com/roie/gohere/internal/runner"
)

func TestPlannedStopIdentityChecks(t *testing.T) {
	for _, mode := range []string{"direct", "wrapper"} {
		for _, identityKind := range []string{"correct", "wrong", "pid-only"} {
			t.Run(mode+"/"+identityKind, func(t *testing.T) {
				oldDetect := detectWSLFunc
				detectWSLFunc = func() bool { return false }
				defer func() { detectWSLFunc = oldDetect }()
				root := t.TempDir()
				start := func(mode string) *runner.Result {
					port, err := runner.ChooseFreePort()
					if err != nil {
						t.Fatal(err)
					}
					result, err := runner.Start(t.Context(), runner.Config{Command: []string{os.Args[0], "-test.run=^TestPlannedProcessHelper$"}, Env: append(os.Environ(), "PORT="+strconv.Itoa(port), "GOHERE_PLANNED_MODE="+mode, "GOHERE_PLANNED_ROOT="+filepath.Join(root, "root.pid"), "GOHERE_PLANNED_CHILD="+filepath.Join(root, "child.pid")), Dir: root, ChosenPort: port, ListenHost: "127.0.0.1", Stdout: io.Discard, Stderr: io.Discard})
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = result.Stop() })
					return result
				}
				unrelated := start("direct")
				result := start(mode)
				identity, ok := lifecycle.ProcessIdentity(result.PID())
				if !ok {
					t.Fatal("identity lookup failed")
				}
				switch identityKind {
				case "wrong":
					identity = "not-the-owned-process"
				case "pid-only":
					identity = ""
				}
				store := router.NewMemoryStore()
				srv := httptest.NewServer(router.NewServer(router.Config{Token: "investigation-only", Store: store}).AdminHandler())
				defer srv.Close()
				client := admin.NewClient(srv.URL, "investigation-only")
				oldClient := defaultAdminClientFunc
				defaultAdminClientFunc = func() (adminClient, error) { return client, nil }
				defer func() { defaultAdminClientFunc = oldClient }()
				reservation, err := client.ReserveRoutes(t.Context(), router.ReservationRequest{RunID: "control", Routes: []router.RouteReservation{{DesiredHost: "controlled.localhost", PreferredScheme: "http", Target: fmt.Sprintf("http://127.0.0.1:%d", result.Port), CWD: root, OwnerCWD: root, OwnerEnv: runOwnerEnv()}}})
				if err != nil {
					t.Fatal(err)
				}
				active, err := client.ActivateRoutes(t.Context(), "control", reservation.PendingRefs(), []router.RouteProcess{{Ref: reservation.PendingRefs()[0], PID: result.PID(), ProcessIdentity: "initial"}})
				if err != nil {
					t.Fatal(err)
				}
				if err := router.UpdateStore(store, func(routes []router.Route) ([]router.Route, error) {
					routes[0].ProcessIdentity = identity
					routes[0].StartedAt = time.Now().Add(-time.Minute)
					return routes, nil
				}); err != nil {
					t.Fatal(err)
				}
				active, _ = client.Routes(t.Context())
				t.Logf("route verified=%t rootPID=%d identityKind=%s", lifecycle.RouteProcessVerified(active[0]), result.PID(), identityKind)
				var output strings.Builder
				if err := StopWithCommand(context.Background(), cli.Command{Kind: cli.CommandStop}, root, &output); err != nil {
					t.Fatal(err)
				}
				deadline := time.Now().Add(3 * time.Second)
				for identityKind == "correct" && plannedServerReachable(result.Port) && time.Now().Before(deadline) {
					time.Sleep(20 * time.Millisecond)
				}
				reachable := plannedServerReachable(result.Port)
				routes, err := client.Routes(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("output=%q reachable=%t routes=%d unrelatedReachable=%t", output.String(), reachable, len(routes), plannedServerReachable(unrelated.Port))
				if identityKind == "correct" {
					if reachable || len(routes) != 0 {
						t.Error("correct identity did not stop root/child and remove route")
					}
				} else {
					if !reachable || len(routes) != 1 {
						t.Error("unverified process or route was removed")
					}
				}
				if !plannedServerReachable(unrelated.Port) {
					t.Error("unrelated server was terminated")
				}
			})
		}
	}
}
