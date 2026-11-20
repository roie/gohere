package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/roie/gohere/internal/admin"
	"github.com/roie/gohere/internal/companion"
	"github.com/roie/gohere/internal/lifecycle"
	"github.com/roie/gohere/internal/router"
)

type pendingStartupOutput chan struct{}

func (w pendingStartupOutput) Write(p []byte) (int, error) {
	select {
	case w <- struct{}{}:
	default:
	}
	return len(p), nil
}

func TestPendingStartupProcessHelper(t *testing.T) {
	delay := os.Getenv("GOHERE_PENDING_STARTUP_DELAY")
	if delay == "" {
		return
	}
	if err := os.WriteFile(os.Getenv("GOHERE_PLANNED_ROOT"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		t.Fatal(err)
	}
	duration, err := time.ParseDuration(delay)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(duration)
	TestPlannedProcessHelper(t)
}

func TestPendingStartup(t *testing.T) {
	useRealGroupRunners(t)
	for _, grouped := range []bool{false, true} {
		for _, bridge := range []bool{false, true} {
			for _, scenario := range []string{"slow", "ownership-loss", "renewal-error"} {
				t.Run(fmt.Sprintf("group=%t/companion=%t/%s", grouped, bridge, scenario), func(t *testing.T) {
					t.Parallel()
					root := t.TempDir()
					items := []multiRunItem{plannedProcessItem(t, root, 0)}
					if grouped {
						items = append(items, plannedProcessItem(t, root, 1))
					}
					for i := range items {
						delay := "32s"
						if grouped && i == 0 {
							delay = "100ms"
						}
						items[i].plan.Command = []string{os.Args[0], "-test.run=^TestPendingStartupProcessHelper$"}
						items[i].plan.Env = append(items[i].plan.Env, "GOHERE_PLANNED_MODE=direct", "GOHERE_PENDING_STARTUP_DELAY="+delay)
						if bridge {
							items[i].plan.OwnerEnv, items[i].plan.OwnerInstance = "wsl", "test-owner"
							items[i].plan.Distro, items[i].plan.LinuxUser = "test-distro", "test-user"
						}
					}
					store := router.NewMemoryStore()
					external := router.Route{ID: "external", Generation: 7, Host: "external.localhost", State: router.RouteStateActive, Target: "http://127.0.0.1:45011"}
					if err := store.Save([]router.Route{external}); err != nil {
						t.Fatal(err)
					}
					handler := router.NewServer(router.Config{Token: "test-token", Store: store}).AdminHandler()
					var pendingRenewals, activations atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if strings.HasSuffix(r.URL.Path, "/renew") {
							routes, _ := store.Load()
							pending := false
							for _, route := range routes {
								pending = pending || route.EffectiveState() == router.RouteStatePending
							}
							if pending && pendingRenewals.Add(1) == 1 {
								switch scenario {
								case "renewal-error":
									http.Error(w, "renewal unavailable", http.StatusServiceUnavailable)
									return
								case "ownership-loss":
									for i := range routes {
										if routes[i].EffectiveState() == router.RouteStatePending {
											routes[i].ID = "replacement-" + routes[i].ID
											routes[i].Generation++
											routes[i].RunID = "replacement"
										}
									}
									if err := store.Save(routes); err != nil {
										t.Error(err)
									}
								}
							}
						}
						if strings.HasSuffix(r.URL.Path, "/activate") {
							activations.Add(1)
						}
						handler.ServeHTTP(w, r)
					}))
					defer server.Close()
					local := admin.NewClient(server.URL, "test-token")
					var client adminClient = local
					if bridge {
						authority := newCompanionAuthority(CompanionConfig{GOOS: "windows", StateDir: t.TempDir(), AdminClient: func() (adminClient, error) { return local, nil }})
						client = &companion.Client{Binary: "test-companion", Runner: activationCompanionRunner{authority: authority}}
					}
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					done := make(chan struct{})
					var runErr error
					start := time.Now()
					output := make(pendingStartupOutput, 1)
					go func() {
						defer close(done)
						if grouped {
							runErr = runPlannedGroup(ctx, false, items, client, output, io.Discard)
						} else {
							runErr = runPlannedSingle(ctx, items[0].cmd, items[0].plan, client, output, io.Discard)
						}
					}()
					defer func() {
						cancel()
						select {
						case <-done:
						case <-time.After(10 * time.Second):
							t.Error("launch cleanup did not join")
						}
					}()
					deadline := time.NewTimer(45 * time.Second)
					defer deadline.Stop()
					ticker := time.NewTicker(20 * time.Millisecond)
					defer ticker.Stop()
					finished := false
					for !finished {
						select {
						case <-done:
							finished = true
						case <-deadline.C:
							t.Fatal("startup cancellation or activation timed out")
						case <-ticker.C:
							if scenario != "slow" {
								continue
							}
							routes, _ := store.Load()
							active := 0
							for _, route := range routes {
								if route.ID != "external" && route.EffectiveState() == router.RouteStateActive {
									active++
									if route.PID <= 0 || route.ProcessIdentity == "" {
										t.Error("activation lost process identity")
									}
								}
							}
							if active == len(items) {
								select {
								case <-output:
									cancel()
								default:
								}
							}
						}
					}
					t.Logf("elapsed=%v pending renewals=%d activations=%d result=%v", time.Since(start), pendingRenewals.Load(), activations.Load(), runErr)
					if scenario == "slow" {
						if runErr != nil || activations.Load() != 1 || pendingRenewals.Load() < 3 {
							t.Fatalf("slow launch failed: %v", runErr)
						}
					} else {
						if runErr == nil || !strings.Contains(runErr.Error(), "could not renew pending route reservation") {
							t.Errorf("renewal failure lost: %v", runErr)
						}
						if activations.Load() != 0 || time.Since(start) > 25*time.Second {
							t.Error("lost reservation did not promptly cancel startup")
						}
					}
					for _, item := range items {
						data, err := os.ReadFile(filepath.Join(item.plan.CWD, "root.pid"))
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
						if lifecycle.PIDAlive(pid) || plannedServerReachable(item.plan.Port) {
							t.Errorf("owned process leaked: pid=%d", pid)
						}
					}
					routes, err := store.Load()
					if err != nil {
						t.Fatal(err)
					}
					want := 1
					if scenario == "ownership-loss" {
						want += len(items)
					}
					if len(routes) != want {
						t.Errorf("wrong cleanup: %+v", routes)
					}
					foundExternal := false
					for _, route := range routes {
						if route.ID == external.ID {
							foundExternal = true
							if route != external {
								t.Error("external route changed")
							}
						} else if route.RunID != "replacement" {
							t.Errorf("owned reservation leaked: %+v", route)
						}
					}
					if !foundExternal {
						t.Error("external route removed")
					}
				})
			}
		}
	}
}
