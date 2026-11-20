package app

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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

func TestPlannedProcessHelper(t *testing.T) {
	mode := os.Getenv("GOHERE_PLANNED_MODE")
	if mode == "" {
		return
	}
	if mode == "wrapper" {
		child := exec.Command(os.Args[0], "-test.run=^TestPlannedProcessHelper$")
		child.Env = append(os.Environ(), "GOHERE_PLANNED_MODE=server")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			panic(err)
		}
		_ = os.WriteFile(os.Getenv("GOHERE_PLANNED_ROOT"), []byte(strconv.Itoa(os.Getpid())), 0600)
		if err := child.Wait(); err != nil {
			os.Exit(1)
		}
		return
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:"+os.Getenv("PORT"))
	if err != nil {
		panic(err)
	}
	if mode == "server" {
		_ = os.WriteFile(os.Getenv("GOHERE_PLANNED_CHILD"), []byte(strconv.Itoa(os.Getpid())), 0600)
	}
	if mode == "direct" {
		_ = os.WriteFile(os.Getenv("GOHERE_PLANNED_ROOT"), []byte(strconv.Itoa(os.Getpid())), 0600)
	}
	fmt.Printf("Local: http://127.0.0.1:%s\n", os.Getenv("PORT"))
	_ = http.Serve(listener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "planned-owned-server") }))
}

func plannedServerReachable(port int) bool {
	c := &http.Client{Timeout: time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b) == "planned-owned-server"
}

func TestPlannedLaunchStop(t *testing.T) {
	useRealGroupRunners(t)
	for _, grouped := range []bool{false, true} {
		for _, mode := range []string{"direct", "wrapper"} {
			label := fmt.Sprintf("group=%t/%s", grouped, mode)
			t.Run(label, func(t *testing.T) {
				oldDetect := detectWSLFunc
				detectWSLFunc = func() bool { return false }
				defer func() { detectWSLFunc = oldDetect }()
				store := router.NewMemoryStore()
				srv := httptest.NewServer(router.NewServer(router.Config{Token: "investigation-only", Store: store}).AdminHandler())
				defer srv.Close()
				client := admin.NewClient(srv.URL, "investigation-only")
				oldClient := defaultAdminClientFunc
				defaultAdminClientFunc = func() (adminClient, error) { return client, nil }
				defer func() { defaultAdminClientFunc = oldClient }()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				count := 1
				if grouped {
					count = 2
				}
				root := t.TempDir()
				items := make([]multiRunItem, count)
				for i := range count {
					port, err := runner.ChooseFreePort()
					if err != nil {
						t.Fatal(err)
					}
					dir := filepath.Join(root, fmt.Sprintf("service%d", i))
					if err := os.Mkdir(dir, 0700); err != nil {
						t.Fatal(err)
					}
					cmd := cli.Command{Kind: cli.CommandRun, Script: fmt.Sprintf("dev%d", i), HTTP: true}
					plan := RunPlan{Command: []string{os.Args[0], "-test.run=^TestPlannedProcessHelper$"},
						Env: append(os.Environ(), "GOHERE_PLANNED_MODE="+mode, "GOHERE_PLANNED_ROOT="+filepath.Join(dir, "root.pid"), "GOHERE_PLANNED_CHILD="+filepath.Join(dir, "child.pid")),
						CWD: dir, Name: fmt.Sprintf("service%d", i), Host: fmt.Sprintf("service%d.localhost", i), Port: port,
						URLScheme: "http", RouteTargetHost: "127.0.0.1", ListenHost: "127.0.0.1", ManagedPort: true, ProjectRoot: root, ProjectName: "plannedworkspace"}
					items[i] = multiRunItem{cmd: cmd, plan: plan}
				}
				done := make(chan error, 1)
				go func() {
					if grouped {
						done <- runPlannedGroup(ctx, false, items, client, io.Discard, io.Discard)
					} else {
						done <- runPlannedSingle(ctx, items[0].cmd, items[0].plan, client, io.Discard, io.Discard)
					}
				}()
				defer func() {
					cancel()
					select {
					case <-done:
					case <-time.After(15 * time.Second):
						t.Error("owned launcher cleanup timed out")
					}
					for _, item := range items {
						if plannedServerReachable(item.plan.Port) {
							t.Errorf("owned server still reachable after cancellation: %d", item.plan.Port)
						}
					}
				}()
				deadline := time.Now().Add(20 * time.Second)
				var routes []router.Route
				for {
					var err error
					routes, err = client.Routes(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					ready := len(routes) == count
					for _, r := range routes {
						ready = ready && r.State == router.RouteStateActive
					}
					if ready {
						break
					}
					select {
					case err := <-done:
						done <- err
						t.Fatalf("launch failed before activation: %v", err)
					default:
					}
					if time.Now().After(deadline) {
						t.Fatalf("activation timeout: %+v", routes)
					}
					time.Sleep(25 * time.Millisecond)
				}
				for i, r := range routes {
					rootBytes, err := os.ReadFile(filepath.Join(items[i].plan.CWD, "root.pid"))
					if err != nil {
						t.Fatal(err)
					}
					pid, _ := strconv.Atoi(string(rootBytes))
					identity, valid := lifecycle.ProcessIdentity(pid)
					childPID := pid
					if b, err := os.ReadFile(filepath.Join(items[i].plan.CWD, "child.pid")); err == nil {
						childPID, _ = strconv.Atoi(string(b))
					}
					if !valid || r.PID != pid || r.ProcessIdentity != identity {
						t.Fatalf("activation lost root identity: route=%+v rootPID=%d identity=%q listenerPID=%d", r, pid, identity, childPID)
					}
				}
				var output strings.Builder
				command := cli.Command{Kind: cli.CommandStop}
				cwd := items[0].plan.CWD
				if grouped {
					command.StopTarget = "plannedworkspace"
					cwd = root
				}
				if err := StopWithCommand(t.Context(), command, cwd, &output); err != nil {
					t.Fatal(err)
				}
				deadline = time.Now().Add(5 * time.Second)
				for {
					remaining, err := client.Routes(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					if len(remaining) == 0 {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("stop left routes: %+v, output=%q", remaining, output.String())
					}
					time.Sleep(20 * time.Millisecond)
				}
				for _, item := range items {
					deadline := time.Now().Add(5 * time.Second)
					for plannedServerReachable(item.plan.Port) && time.Now().Before(deadline) {
						time.Sleep(20 * time.Millisecond)
					}
					if plannedServerReachable(item.plan.Port) {
						t.Errorf("stop removed route but server still responds on port %d", item.plan.Port)
					}
				}
			})
		}
	}
}
