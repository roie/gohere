package runner

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/roie/gohere/internal/project"
)

func TestChooseFreePortReturnsFreePort(t *testing.T) {
	port, err := ChooseFreePort()
	if err != nil {
		t.Fatal(err)
	}
	if port <= 0 || port > 65535 {
		t.Fatalf("port = %d", port)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatalf("chosen port should be available: %v", err)
	}
	ln.Close()
}

func TestChildEnvSetsHiddenPortAndHost(t *testing.T) {
	env := ChildEnv([]string{"PATH=/bin", "PORT=3000"}, 49231)

	assertEnv(t, env, "PATH", "/bin")
	assertEnv(t, env, "PORT", "49231")
	assertEnv(t, env, "HOST", "127.0.0.1")
	assertEnv(t, env, "NUXT_PORT", "49231")
	assertEnv(t, env, "NUXT_HOST", "127.0.0.1")
}

func TestPortFlagConflictDetection(t *testing.T) {
	tests := []struct {
		command string
		want    bool
	}{
		{"vite --port 5173", true},
		{"vite --port=5173", true},
		{"next dev -p 3001", true},
		{"vite --host 0.0.0.0", true},
		{"astro dev --hostname 127.0.0.1", true},
		{"vite", false},
	}

	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			if got := HasExplicitPortOrHostFlag(tt.command); got != tt.want {
				t.Fatalf("HasExplicitPortOrHostFlag(%q) = %v, want %v", tt.command, got, tt.want)
			}
		})
	}
}

func TestInjectPortArgs(t *testing.T) {
	tests := []struct {
		name     string
		command  string
		portFlag string
		want     []string
	}{
		{"vite", "vite --clearScreen false", "", []string{"--", "--host", "127.0.0.1", "--port", "49231", "--strictPort"}},
		{"svelte kit", "svelte-kit dev", "", []string{"--", "--host", "127.0.0.1", "--port", "49231", "--strictPort"}},
		{"astro", "astro dev", "", []string{"--", "--host", "127.0.0.1", "--port", "49231", "--strictPort"}},
		{"vp like", "vp dev", "", []string{"--", "--host", "127.0.0.1", "--port", "49231", "--strictPort"}},
		{"next", "next dev", "", []string{"--", "-p", "49231"}},
		{"nuxt", "nuxt dev", "", []string{"--", "--host", "127.0.0.1", "--port", "49231"}},
		{"wrangler", "wrangler dev", "", []string{"--", "--port", "49231"}},
		{"explicit flag wins", "vite --port 3000", "", nil},
		{"escape hatch", "custom dev", "--listen", []string{"--", "--listen", "49231"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := InjectPortArgs(tt.command, 49231, tt.portFlag)
			if !sameStrings(got, tt.want) {
				t.Fatalf("InjectPortArgs() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestBuildScriptCommand(t *testing.T) {
	tests := []struct {
		pm   project.PackageManager
		want []string
	}{
		{project.PackageManagerNPM, []string{"npm", "run", "dev", "--", "--port", "49231"}},
		{project.PackageManagerPNPM, []string{"pnpm", "run", "dev", "--port", "49231"}},
		{project.PackageManagerYarn, []string{"yarn", "dev", "--port", "49231"}},
		{project.PackageManagerBun, []string{"bun", "run", "dev", "--port", "49231"}},
	}

	for _, tt := range tests {
		t.Run(string(tt.pm), func(t *testing.T) {
			got := BuildScriptCommand(tt.pm, "dev", []string{"--", "--port", "49231"})
			if !sameStrings(got, tt.want) {
				t.Fatalf("BuildScriptCommand() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestDetectPortFromOutput(t *testing.T) {
	tests := []struct {
		line string
		want int
		ok   bool
	}{
		{"Local: http://localhost:5173/", 5173, true},
		{"ready on http://127.0.0.1:49231", 49231, true},
		{"listening at http://0.0.0.0:8080", 8080, true},
		{"no url here", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			got, ok := DetectPortFromOutput(tt.line)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("DetectPortFromOutput() = %d, %v; want %d, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestRunStreamsOutputAndDetectsPort(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cfg := Config{
		Command:        []string{os.Args[0], "-test.run=TestHelperProcess", "--", "print-port"},
		Env:            []string{"GOHERE_HELPER_PROCESS=1"},
		Stdout:         &stdout,
		Stderr:         &stderr,
		StartupTimeout: 2 * time.Second,
	}

	result, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Stop()

	if result.Port != 47654 {
		t.Fatalf("detected port = %d, want 47654", result.Port)
	}
	if !strings.Contains(stdout.String(), "http://127.0.0.1:47654") {
		t.Fatalf("stdout was not streamed: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "diagnostic line") {
		t.Fatalf("stderr was not streamed: %q", stderr.String())
	}
}

func TestWaitTreatsContextCancelAsCleanShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	result, err := Start(ctx, Config{
		Command:        []string{os.Args[0], "-test.run=TestHelperProcess", "--", "print-port-sleep"},
		Env:            []string{"GOHERE_HELPER_PROCESS=1"},
		Stdout:         &bytes.Buffer{},
		Stderr:         &bytes.Buffer{},
		StartupTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	cancel()
	if err := result.Wait(); err != nil {
		t.Fatalf("Wait after context cancel = %v, want nil", err)
	}
}

func TestRunFallsBackToChosenPortWhenReachable(t *testing.T) {
	server := &http.Server{Addr: "127.0.0.1:0"}
	ln, err := net.Listen("tcp", server.Addr)
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	go server.Serve(ln)
	defer server.Close()

	cfg := Config{
		Command:        []string{os.Args[0], "-test.run=TestHelperProcess", "--", "sleep"},
		Env:            []string{"GOHERE_HELPER_PROCESS=1"},
		ChosenPort:     port,
		Stdout:         &bytes.Buffer{},
		Stderr:         &bytes.Buffer{},
		StartupTimeout: 10 * time.Millisecond,
	}

	result, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Stop()

	if result.Port != port {
		t.Fatalf("fallback port = %d, want %d", result.Port, port)
	}
}

func TestRunCanRequireDetectedPort(t *testing.T) {
	server := &http.Server{Addr: "127.0.0.1:0"}
	ln, err := net.Listen("tcp", server.Addr)
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	go server.Serve(ln)
	defer server.Close()

	cfg := Config{
		Command:             []string{os.Args[0], "-test.run=TestHelperProcess", "--", "sleep"},
		Env:                 []string{"GOHERE_HELPER_PROCESS=1"},
		ChosenPort:          port,
		RequireDetectedPort: true,
		Stdout:              &bytes.Buffer{},
		Stderr:              &bytes.Buffer{},
		StartupTimeout:      10 * time.Millisecond,
	}

	if _, err := Start(context.Background(), cfg); err == nil {
		t.Fatal("expected no detected port error")
	}
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("GOHERE_HELPER_PROCESS") != "1" {
		return
	}

	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) < 2 {
		os.Exit(2)
	}

	switch args[1] {
	case "print-port":
		os.Stderr.WriteString("diagnostic line\n")
		os.Stdout.WriteString("Local: http://127.0.0.1:47654\n")
	case "print-port-sleep":
		os.Stdout.WriteString("Local: http://127.0.0.1:47654\n")
		time.Sleep(2 * time.Second)
	case "sleep":
		time.Sleep(2 * time.Second)
	default:
		os.Exit(2)
	}
	os.Exit(0)
}

func assertEnv(t *testing.T, env []string, key, want string) {
	t.Helper()
	prefix := key + "="
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			if got := strings.TrimPrefix(item, prefix); got != want {
				t.Fatalf("%s = %q, want %q", key, got, want)
			}
			return
		}
	}
	t.Fatalf("missing env %s", key)
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
