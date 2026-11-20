package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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

func useRealGroupRunners(t *testing.T) {
	t.Helper()
	original := launchGroupRunnerFunc
	launchGroupRunnerFunc = func(ctx context.Context, cfg runner.Config) (groupRunner, error) {
		result, err := runner.Launch(ctx, cfg)
		if err != nil {
			return nil, err
		}
		return &launchedGroupRunner{result: result}, nil
	}
	t.Cleanup(func() { launchGroupRunnerFunc = original })
}

func plannedProcessItem(t *testing.T, root string, index int) multiRunItem {
	t.Helper()
	port, err := runner.ChooseFreePort()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, fmt.Sprintf("service%d", index))
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return multiRunItem{cmd: cli.Command{Kind: cli.CommandRun, Script: fmt.Sprintf("dev%d", index), HTTP: true}, plan: RunPlan{
		Command: []string{os.Args[0], "-test.run=^TestPlannedProcessHelper$"},
		Env:     append(os.Environ(), "GOHERE_PLANNED_MODE=wrapper", "GOHERE_PLANNED_ROOT="+filepath.Join(dir, "root.pid"), "GOHERE_PLANNED_CHILD="+filepath.Join(dir, "child.pid")),
		CWD:     dir, Name: fmt.Sprintf("service%d", index), Host: fmt.Sprintf("service%d.localhost", index), Port: port,
		URLScheme: "http", RouteTargetHost: "127.0.0.1", ListenHost: "127.0.0.1", ManagedPort: true, ProjectRoot: root, ProjectName: "workspace",
	}}
}

func TestPlannedLaunchFailureStopsOnlyNewProcesses(t *testing.T) {
	useRealGroupRunners(t)
	for _, grouped := range []bool{false, true} {
		for _, failure := range []string{"identity", "activation", "older-api"} {
			t.Run(fmt.Sprintf("group=%t/%s", grouped, failure), func(t *testing.T) {
				root := t.TempDir()
				reused := plannedProcessItem(t, root, 0)
				result, err := runner.Start(t.Context(), runner.Config{Command: reused.plan.Command, Dir: reused.plan.CWD, Env: append(reused.plan.Env, "PORT="+strconv.Itoa(reused.plan.Port)), ChosenPort: reused.plan.Port, ListenHost: "127.0.0.1", Stdout: io.Discard, Stderr: io.Discard})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = result.Stop() })
				identity, ok := lifecycle.ProcessIdentity(result.PID())
				if !ok {
					t.Fatal("missing unrelated process identity")
				}
				existing := router.Route{ID: "reused", Generation: 3, State: router.RouteStateActive, Host: reused.plan.Host, PreferredScheme: "http", Target: routeTarget("127.0.0.1", result.Port), CWD: reused.plan.CWD, PID: result.PID(), ProcessIdentity: identity, OwnerEnv: runOwnerEnv()}
				store := router.NewMemoryStore()
				if err := store.Save([]router.Route{existing}); err != nil {
					t.Fatal(err)
				}
				handler := router.NewServer(router.Config{Token: "test-token", Store: store}).AdminHandler()
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasPrefix(r.URL.Path, "/v3/") {
						status := http.StatusInternalServerError
						if failure == "older-api" {
							status = http.StatusNotFound
						}
						http.Error(w, "activation rejected", status)
						return
					}
					handler.ServeHTTP(w, r)
				}))
				defer srv.Close()
				client := admin.NewClient(srv.URL, "test-token")
				if failure == "identity" {
					original := activationProcess
					captured := 0
					activationProcess = func(ref router.RouteRef, pid int) (router.RouteProcess, error) {
						captured++
						if grouped && captured == 1 {
							return original(ref, pid)
						}
						return router.RouteProcess{}, errors.New("identity capture rejected")
					}
					defer func() { activationProcess = original }()
				}
				items := []multiRunItem{plannedProcessItem(t, root, 1)}
				if grouped {
					items = append(items, plannedProcessItem(t, root, 2), reused)
				}
				if grouped {
					err = runPlannedGroup(t.Context(), false, items, client, io.Discard, io.Discard)
				} else {
					err = runPlannedSingle(t.Context(), items[0].cmd, items[0].plan, client, io.Discard, io.Discard)
				}
				if err == nil {
					t.Fatal("launch unexpectedly succeeded")
				}
				routes, err := store.Load()
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(routes, []router.Route{existing}) {
					t.Errorf("failed launch changed reused/external route: %+v", routes)
				}
				if !plannedServerReachable(result.Port) {
					t.Error("failed launch stopped reused/external process")
				}
				for _, item := range items {
					if item.plan.CWD == reused.plan.CWD {
						continue
					}
					for _, name := range []string{"root.pid", "child.pid"} {
						data, err := os.ReadFile(filepath.Join(item.plan.CWD, name))
						if err != nil {
							t.Fatal(err)
						}
						pid, err := strconv.Atoi(string(data))
						if err != nil {
							t.Fatal(err)
						}
						deadline := time.Now().Add(5 * time.Second)
						for lifecycle.PIDAlive(pid) && time.Now().Before(deadline) {
							time.Sleep(20 * time.Millisecond)
						}
						if lifecycle.PIDAlive(pid) {
							t.Errorf("failed launch left %s process %d alive", name, pid)
						}
					}
					if plannedServerReachable(item.plan.Port) {
						t.Errorf("failed launch left server on port %d", item.plan.Port)
					}
				}
			})
		}
	}
}
