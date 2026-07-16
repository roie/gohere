package runner

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
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

func TestChooseFreePortForHostReturnsFreePortOnRequestedHost(t *testing.T) {
	port, err := ChooseFreePortForHost("0.0.0.0")
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "0.0.0.0:"+strconv.Itoa(port))
	if err != nil {
		t.Fatalf("port %d should be free on 0.0.0.0: %v", port, err)
	}
	ln.Close()
}

func TestListenerPortRejectsNonTCPListener(t *testing.T) {
	_, err := listenerPort(nonTCPListener{})
	if err == nil {
		t.Fatal("expected non-TCP listener error")
	}
	if !strings.Contains(err.Error(), "not TCP") {
		t.Fatalf("error = %q, want not TCP", err.Error())
	}
}

func TestChildEnvSetsHiddenPortAndHost(t *testing.T) {
	env := ChildEnv([]string{"PATH=/bin", "PORT=3000"}, 49231)

	assertEnv(t, env, "PATH", "/bin")
	assertEnv(t, env, "PORT", "49231")
	assertEnv(t, env, "HOST", "127.0.0.1")
	assertEnv(t, env, "NUXT_PORT", "49231")
	assertEnv(t, env, "NUXT_HOST", "127.0.0.1")
}

func TestChildEnvForHostSetsCustomHost(t *testing.T) {
	env := ChildEnvForHost([]string{"PATH=/bin", "HOST=127.0.0.1"}, 49231, "0.0.0.0")

	assertEnv(t, env, "PATH", "/bin")
	assertEnv(t, env, "PORT", "49231")
	assertEnv(t, env, "HOST", "0.0.0.0")
	assertEnv(t, env, "NUXT_PORT", "49231")
	assertEnv(t, env, "NUXT_HOST", "0.0.0.0")
}

func TestPortFlagConflictDetection(t *testing.T) {
	tests := []struct {
		command string
		want    bool
	}{
		{"vite --port 5173", true},
		{"vite --port=5173", true},
		{"next dev -p 3001", true},
		{"next dev -p3002", true},
		{"custom -profile dev", false},
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

func TestExplicitPort(t *testing.T) {
	for _, test := range []struct {
		command string
		port    int
		ok      bool
	}{
		{"vite --port 5173", 5173, true},
		{"vite --port=4173", 4173, true},
		{"next dev -p 3001", 3001, true},
		{"next dev -p3002", 3002, true},
		{"vite", 0, false},
		{"vite --port nope", 0, false},
	} {
		port, ok := ExplicitPort(test.command)
		if port != test.port || ok != test.ok {
			t.Fatalf("ExplicitPort(%q) = %d/%v, want %d/%v", test.command, port, ok, test.port, test.ok)
		}
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

func TestInjectPortArgsForHostAddsHostWhenOnlyPortIsExplicit(t *testing.T) {
	got := InjectPortArgsForHost("vite --port 3000", 49231, "", "0.0.0.0")
	want := []string{"--", "--host", "0.0.0.0"}
	if !sameStrings(got, want) {
		t.Fatalf("InjectPortArgsForHost() = %#v, want %#v", got, want)
	}

	got = InjectPortArgsForHost("vite --port 3000 --host 127.0.0.1", 49231, "", "0.0.0.0")
	if got != nil {
		t.Fatalf("explicit host should still win, got %#v", got)
	}
}

func TestInjectPortArgsForHost(t *testing.T) {
	got := InjectPortArgsForHost("vite --clearScreen false", 49231, "", "0.0.0.0")
	want := []string{"--", "--host", "0.0.0.0", "--port", "49231", "--strictPort"}
	if !sameStrings(got, want) {
		t.Fatalf("InjectPortArgsForHost() = %#v, want %#v", got, want)
	}
}

func TestInjectPortArgsDetectsEnvPrefixedTool(t *testing.T) {
	got := InjectPortArgsForHost("NODE_ENV=production vite --clearScreen false", 49231, "", "127.0.0.1")
	want := []string{"--", "--host", "127.0.0.1", "--port", "49231", "--strictPort"}
	if !sameStrings(got, want) {
		t.Fatalf("InjectPortArgsForHost() = %#v, want %#v", got, want)
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

func TestLaunchSeparatesProcessStartFromReadiness(t *testing.T) {
	result, err := Launch(context.Background(), Config{
		Command: []string{os.Args[0], "-test.run=TestHelperProcess", "--", "print-port-sleep"},
		Env:     []string{"GOHERE_HELPER_PROCESS=1"}, StartupTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Stop()
	if result.Port != 0 {
		t.Fatalf("port before readiness = %d, want 0", result.Port)
	}
	if err := result.WaitReady(); err != nil {
		t.Fatal(err)
	}
	if result.Port != 47654 {
		t.Fatalf("ready port = %d, want 47654", result.Port)
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

	if result.Port != 47654 {
		t.Fatalf("detected port = %d, want 47654", result.Port)
	}
	if err := result.Wait(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "http://127.0.0.1:47654") {
		t.Fatalf("stdout was not streamed: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "diagnostic line") {
		t.Fatalf("stderr was not streamed: %q", stderr.String())
	}
}

func TestStartDoesNotDuplicateFullEnvironment(t *testing.T) {
	t.Setenv("GOHERE_DUP_ENV", "parent")
	env := ChildEnv(os.Environ(), 49231)
	env = append(env, "GOHERE_HELPER_PROCESS=1")
	server := &http.Server{Addr: "127.0.0.1:0"}
	ln, err := net.Listen("tcp", server.Addr)
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	go server.Serve(ln)
	defer server.Close()
	var stdout bytes.Buffer

	result, err := Start(context.Background(), Config{
		Command:        []string{os.Args[0], "-test.run=TestHelperProcess", "--", "print-env-count", "GOHERE_DUP_ENV"},
		Env:            env,
		ChosenPort:     port,
		Stdout:         &stdout,
		Stderr:         &bytes.Buffer{},
		StartupTimeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := result.Stop(); err != nil {
		t.Fatal(err)
	}

	if firstNonEmptyLine(stdout.String()) != "1" {
		t.Fatalf("GOHERE_DUP_ENV count = %q, want 1", stdout.String())
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

func TestStopTerminatesChildProcessTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("covered by process tree command selection on Windows")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is required for process tree test")
	}
	dir := t.TempDir()
	childPIDPath := filepath.Join(dir, "child.pid")
	script := "sleep 30 & echo $! > " + childPIDPath + "; echo Local: http://127.0.0.1:47654; wait"
	result, err := Start(context.Background(), Config{
		Command:        []string{"sh", "-c", script},
		Stdout:         &bytes.Buffer{},
		Stderr:         &bytes.Buffer{},
		StartupTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(childPIDPath)
	if err != nil {
		result.Stop()
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		result.Stop()
		t.Fatal(err)
	}

	if err := result.Stop(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processAliveForTest(childPID) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("child process %d is still alive after Stop", childPID)
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

func TestRunWaitsForSlowChosenPortWhenNoURLIsPrinted(t *testing.T) {
	port, err := ChooseFreePort()
	if err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		Command:        []string{os.Args[0], "-test.run=TestHelperProcess", "--", "delayed-listen", strconv.Itoa(port), "150ms"},
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

func TestStartRunsCommandInConfiguredDir(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Command:        []string{os.Args[0], "-test.run=TestHelperProcess", "--", "print-cwd-port"},
		Dir:            dir,
		Env:            []string{"GOHERE_HELPER_PROCESS=1"},
		Stdout:         &bytes.Buffer{},
		Stderr:         &bytes.Buffer{},
		StartupTimeout: 2 * time.Second,
	}

	result, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Stop()

	got := firstNonEmptyLine(cfg.Stdout.(*bytes.Buffer).String())
	canonicalGot, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatal(err)
	}
	canonicalWant, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if canonicalGot != canonicalWant {
		t.Fatalf("child cwd = %q, want %q", got, dir)
	}
}

func TestPortReachableUsesHEADWithoutGET(t *testing.T) {
	methods := make(chan string, 1)
	server := &http.Server{Addr: "127.0.0.1:0", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods <- r.Method
		if r.Method == http.MethodGet {
			t.Fatal("PortReachable should not use GET")
		}
		w.WriteHeader(http.StatusNoContent)
	})}
	ln, err := net.Listen("tcp", server.Addr)
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	go server.Serve(ln)
	defer server.Close()

	if !PortReachable(port) {
		t.Fatal("expected port to be reachable")
	}
	select {
	case method := <-methods:
		if method != http.MethodHead {
			t.Fatalf("method = %q, want HEAD", method)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not receive probe")
	}
}

func TestPortReachableClosesIdleProbeConnection(t *testing.T) {
	closed := make(chan struct{}, 1)
	server := &http.Server{
		Addr: "127.0.0.1:0",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}),
		ConnState: func(conn net.Conn, state http.ConnState) {
			if state == http.StateClosed {
				select {
				case closed <- struct{}{}:
				default:
				}
			}
		},
	}
	ln, err := net.Listen("tcp", server.Addr)
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	go server.Serve(ln)
	defer server.Close()

	if !PortReachable(port) {
		t.Fatal("expected port to be reachable")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("probe connection was not closed")
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

func TestStreamAndScanHandlesLongLines(t *testing.T) {
	line := strings.Repeat("x", 70*1024) + " http://localhost:5173"
	detected := make(chan int, 1)
	var wg sync.WaitGroup
	var out bytes.Buffer
	wg.Add(1)
	streamAndScan(strings.NewReader(line+"\n"), &out, detected, &wg)
	wg.Wait()

	if !strings.Contains(out.String(), "http://localhost:5173") {
		t.Fatal("long line was not copied to output")
	}
	select {
	case port := <-detected:
		if port != 5173 {
			t.Fatalf("port = %d, want 5173", port)
		}
	default:
		t.Fatal("port was not detected from long line")
	}
}

func TestDetectPortFromOutputDoesNotMatchPortInsideWord(t *testing.T) {
	if port, ok := DetectPortFromOutput("http://localhost:3000abc"); ok {
		t.Fatalf("detected port = %d, want no match", port)
	}
}

func TestStartReturnsFinishedWhenProcessExitsZeroBeforeURL(t *testing.T) {
	var stdout bytes.Buffer
	cfg := Config{
		Command:        []string{os.Args[0], "-test.run=TestHelperProcess", "--", "finish-zero"},
		Env:            []string{"GOHERE_HELPER_PROCESS=1"},
		Stdout:         &stdout,
		Stderr:         &bytes.Buffer{},
		StartupTimeout: 5 * time.Second,
	}

	_, err := Start(context.Background(), cfg)
	if !errors.Is(err, ErrProcessFinished) {
		t.Fatalf("error = %v, want ErrProcessFinished", err)
	}
	if !strings.Contains(stdout.String(), "finished ok") {
		t.Fatalf("stdout was not streamed: %q", stdout.String())
	}
}

func TestStartReturnsFailedWhenProcessExitsNonZeroBeforeURL(t *testing.T) {
	cfg := Config{
		Command:        []string{os.Args[0], "-test.run=TestHelperProcess", "--", "finish-one"},
		Env:            []string{"GOHERE_HELPER_PROCESS=1"},
		Stdout:         &bytes.Buffer{},
		Stderr:         &bytes.Buffer{},
		StartupTimeout: 5 * time.Second,
	}

	_, err := Start(context.Background(), cfg)
	if !errors.Is(err, ErrProcessFailed) {
		t.Fatalf("error = %v, want ErrProcessFailed", err)
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
	case "print-cwd-port":
		cwd, err := os.Getwd()
		if err != nil {
			os.Exit(2)
		}
		os.Stdout.WriteString(cwd + "\n")
		os.Stdout.WriteString("Local: http://127.0.0.1:47654\n")
		time.Sleep(2 * time.Second)
	case "sleep":
		time.Sleep(2 * time.Second)
	case "delayed-listen":
		if len(args) < 4 {
			os.Exit(2)
		}
		port, err := strconv.Atoi(args[2])
		if err != nil {
			os.Exit(2)
		}
		delay, err := time.ParseDuration(args[3])
		if err != nil {
			os.Exit(2)
		}
		time.Sleep(delay)
		server := &http.Server{Addr: "127.0.0.1:" + strconv.Itoa(port), Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})}
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			os.Exit(1)
		}
	case "print-env-count":
		key := args[len(args)-1]
		count := 0
		for _, item := range os.Environ() {
			if strings.HasPrefix(item, key+"=") {
				count++
			}
		}
		os.Stdout.WriteString(strconv.Itoa(count) + "\n")
		time.Sleep(2 * time.Second)
	case "finish-zero":
		os.Stdout.WriteString("finished ok\n")
	case "finish-one":
		os.Exit(1)
	default:
		os.Exit(2)
	}
	os.Exit(0)
}

func firstNonEmptyLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
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

type nonTCPListener struct{}

func (nonTCPListener) Accept() (net.Conn, error) { return nil, errors.New("unused") }
func (nonTCPListener) Close() error              { return nil }
func (nonTCPListener) Addr() net.Addr            { return nonTCPAddr("pipe") }

type nonTCPAddr string

func (a nonTCPAddr) Network() string { return string(a) }
func (a nonTCPAddr) String() string  { return string(a) }
