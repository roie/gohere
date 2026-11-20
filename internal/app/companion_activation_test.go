package app

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/roie/gohere/internal/admin"
	"github.com/roie/gohere/internal/companion"
	"github.com/roie/gohere/internal/lifecycle"
	"github.com/roie/gohere/internal/router"
)

type activationCompanionRunner struct{ authority companion.Authority }

func (r activationCompanionRunner) Run(ctx context.Context, _ string, _ []string, stdin []byte) ([]byte, []byte, error) {
	var stdout bytes.Buffer
	err := companion.Serve(ctx, bytes.NewReader(stdin), &stdout, r.authority)
	return stdout.Bytes(), nil, err
}

func TestCompanionPlannedActivationPersistsRootIdentity(t *testing.T) {
	useRealGroupRunners(t)
	for _, grouped := range []bool{false, true} {
		t.Run(map[bool]string{false: "single", true: "group"}[grouped], func(t *testing.T) {
			store := router.NewMemoryStore()
			server := httptest.NewServer(router.NewServer(router.Config{Token: "test-token", Store: store}).AdminHandler())
			defer server.Close()
			local := admin.NewClient(server.URL, "test-token")
			authority := newCompanionAuthority(CompanionConfig{GOOS: "windows", StateDir: t.TempDir(), AdminClient: func() (adminClient, error) { return local, nil }})
			client := &companion.Client{Binary: "test-companion", Runner: activationCompanionRunner{authority: authority}}
			root := t.TempDir()
			items := []multiRunItem{plannedProcessItem(t, root, 0)}
			if grouped {
				items = append(items, plannedProcessItem(t, root, 1))
			}
			for i := range items {
				items[i].plan.OwnerEnv = "wsl"
				items[i].plan.OwnerInstance = "test-owner"
				items[i].plan.Distro = "test-distro"
				items[i].plan.LinuxUser = "test-user"
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan struct{})
			var runErr error
			go func() {
				defer close(done)
				if grouped {
					runErr = runPlannedGroup(ctx, false, items, client, io.Discard, io.Discard)
				} else {
					runErr = runPlannedSingle(ctx, items[0].cmd, items[0].plan, client, io.Discard, io.Discard)
				}
			}()
			defer func() {
				cancel()
				select {
				case <-done:
					if runErr != nil {
						t.Error(runErr)
					}
				case <-time.After(10 * time.Second):
					t.Error("companion launch cleanup timed out")
				}
			}()
			deadline := time.Now().Add(15 * time.Second)
			for {
				routes, err := store.Load()
				if err != nil {
					t.Fatal(err)
				}
				ready := len(routes) == len(items)
				for _, route := range routes {
					ready = ready && route.EffectiveState() == router.RouteStateActive
				}
				if ready {
					for _, route := range routes {
						if route.OwnerEnv != "wsl" || route.OwnerInstance != "test-owner" || route.PID <= 0 || route.ProcessIdentity == "" || !lifecycle.RouteProcessVerified(route) {
							t.Fatalf("companion lost ownership metadata: %+v", route)
						}
					}
					break
				}
				select {
				case <-done:
					t.Fatalf("launch ended before activation: %v", runErr)
				default:
				}
				if time.Now().After(deadline) {
					t.Fatalf("companion activation timed out: %+v", routes)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}
