package runner

import (
	"context"
	"encoding/json/v2"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"testing"
	"time"
)

func TestListenerOwnershipProbe(t *testing.T) {
	if os.Getenv("GOHERE_OWNERSHIP_TEST") != "1" {
		t.Skip("set GOHERE_OWNERSHIP_TEST=1 to run native listener ownership probes")
	}
	for _, wrapper := range []string{"direct", "npm", "pnpm", "detached", "ipv6", "wildcard", "dual"} {
		t.Run(wrapper, func(t *testing.T) {
			host := "127.0.0.1"
			if wrapper == "ipv6" {
				host = "::1"
			}
			bindHost := host
			if wrapper == "wildcard" {
				bindHost = "0.0.0.0"
			}
			if wrapper == "dual" {
				bindHost = "::"
			}
			root, ready := startOwnershipFixture(t, wrapper, bindHost, 0, "intended")
			if ready.Error != "" {
				t.Fatalf("listener startup: %s", ready.Error)
			}
			owners, err := listenerPIDs(t.Context(), host, ready.Port)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(owners, ready.PID) {
				t.Fatalf("listener owners = %v, want fixture PID %d", owners, ready.PID)
			}
			owned, err := listenerOwned(t.Context(), host, ready.Port, root)
			if err != nil || !owned {
				t.Fatalf("owned = %v, error = %v", owned, err)
			}
			if (wrapper == "npm" || wrapper == "pnpm" || wrapper == "detached") && root == ready.PID {
				t.Fatal("wrapper fixture did not produce a descendant listener")
			}
			t.Logf("host=%s root=%d listener=%d owners=%v owned=%v", host, root, ready.PID, owners, owned)
		})
	}

	t.Run("takeover", func(t *testing.T) {
		port, err := ChooseFreePortForHost("127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		_, foreign := startOwnershipFixture(t, "direct", "127.0.0.1", port, "unrelated")
		if foreign.Error != "" {
			t.Fatalf("contender startup: %s", foreign.Error)
		}
		root, failed := startOwnershipFixture(t, "direct", "127.0.0.1", port, "intended")
		if failed.Error != "EADDRINUSE" {
			t.Fatalf("intended child bind error = %q, want EADDRINUSE", failed.Error)
		}
		transport := &http.Transport{Proxy: nil}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
		response, err := client.Get("http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil || string(body) != "unrelated" {
			t.Fatalf("reachable endpoint = %q, error = %v", body, err)
		}
		owned, err := listenerOwned(t.Context(), "127.0.0.1", port, root)
		if err != nil || owned {
			t.Fatalf("foreign listener owned = %v, error = %v", owned, err)
		}
		t.Log("failed-bind child remained alive; reachable unrelated listener was rejected")
	})

	t.Run("missing", func(t *testing.T) {
		owned, err := listenerOwned(t.Context(), "127.0.0.1", 0, os.Getpid())
		if owned || err == nil {
			t.Fatalf("missing listener owned = %v, error = %v; want unknown", owned, err)
		}
	})
}

type ownershipFixture struct {
	PID   int    `json:"pid"`
	Port  int    `json:"port"`
	Error string `json:"error"`
}

func startOwnershipFixture(t *testing.T, wrapper, host string, port int, label string) (int, ownershipFixture) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"package.json": `{"private":true,"scripts":{"serve":"node server.cjs"}}`,
		"server.cjs":   ownershipFixtureJS,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	command := []string{"node", "server.cjs"}
	if wrapper == "npm" || wrapper == "pnpm" {
		if _, err := exec.LookPath(wrapper); err != nil {
			t.Fatal(err)
		}
		command = []string{wrapper, "run", "--silent", "serve"}
		if runtime.GOOS == "windows" {
			command = []string{"cmd.exe", "/d", "/c", wrapper + " run --silent serve"}
		}
	}
	marker := filepath.Join(dir, "ready.json")
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 20*time.Second)
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GOHERE_PROBE_HOST="+host, "GOHERE_PROBE_PORT="+strconv.Itoa(port),
		"GOHERE_PROBE_MARKER="+marker, "GOHERE_PROBE_LABEL="+label,
		"GOHERE_PROBE_WRAPPER="+wrapper)
	configureProcessTree(cmd)
	cmd.Cancel = func() error { return terminateProcessTree(cmd) }
	cmd.WaitDelay = 2 * time.Second
	logPath := filepath.Join(dir, "output.log")
	log, err := os.Create(logPath)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		cancel()
		log.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		terminateProcessTree(cmd)
		cancel()
		cmd.Wait()
		log.Close()
	})
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(marker)
		var ready ownershipFixture
		if err == nil && json.Unmarshal(data, &ready) == nil && ready.PID > 0 {
			process, err := os.FindProcess(ready.PID)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { process.Kill(); process.Release() })
			return cmd.Process.Pid, ready
		}
		time.Sleep(20 * time.Millisecond)
	}
	output, _ := os.ReadFile(logPath)
	t.Fatalf("fixture did not become ready: %.2000s", output)
	return 0, ownershipFixture{}
}

const ownershipFixtureJS = `
const fs = require('node:fs');
const http = require('node:http');
if (process.env.GOHERE_PROBE_WRAPPER === 'detached') {
  require('node:child_process').spawn(process.execPath, [__filename], {
    detached: true, stdio: 'ignore',
    env: {...process.env, GOHERE_PROBE_WRAPPER: 'direct'},
  });
  setInterval(() => {}, 1000);
} else {
  const port = Number(process.env.GOHERE_PROBE_PORT);
  const report = value => fs.writeFileSync(process.env.GOHERE_PROBE_MARKER,
    JSON.stringify({pid: process.pid, ...value}));
  const server = http.createServer((req, res) => res.end(process.env.GOHERE_PROBE_LABEL));
  server.on('error', error => {
    report({port, error: error.code});
    setInterval(() => {}, 1000);
  });
  server.listen(port, process.env.GOHERE_PROBE_HOST, () => report({port: server.address().port}));
}
`
