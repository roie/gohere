package runner

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/roie/gohere/internal/project"
)

type Config struct {
	Command             []string
	Env                 []string
	ChosenPort          int
	RequireDetectedPort bool
	Stdout              io.Writer
	Stderr              io.Writer
	StartupTimeout      time.Duration
}

type Result struct {
	Port    int
	ctx     context.Context
	cmd     *exec.Cmd
	done    chan struct{}
	waitErr error
}

func (r *Result) Stop() error {
	if r == nil || r.cmd == nil || r.cmd.Process == nil {
		return nil
	}
	if err := r.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	<-r.done
	return nil
}

func (r *Result) Wait() error {
	if r == nil || r.done == nil {
		return nil
	}
	<-r.done
	if r.waitErr != nil && r.ctx != nil && r.ctx.Err() != nil {
		return nil
	}
	return r.waitErr
}

func (r *Result) PID() int {
	if r == nil || r.cmd == nil || r.cmd.Process == nil {
		return 0
	}
	return r.cmd.Process.Pid
}

func ChooseFreePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()

	return ln.Addr().(*net.TCPAddr).Port, nil
}

func ChildEnv(base []string, port int) []string {
	return ChildEnvForHost(base, port, "127.0.0.1")
}

func ChildEnvForHost(base []string, port int, host string) []string {
	if host == "" {
		host = "127.0.0.1"
	}
	portValue := strconv.Itoa(port)
	overrides := map[string]string{
		"PORT":      portValue,
		"HOST":      host,
		"NUXT_PORT": portValue,
		"NUXT_HOST": host,
	}

	out := make([]string, 0, len(base)+len(overrides))
	seen := make(map[string]bool, len(overrides))
	for _, item := range base {
		key, _, ok := strings.Cut(item, "=")
		if !ok {
			out = append(out, item)
			continue
		}
		if value, ok := overrides[key]; ok {
			out = append(out, key+"="+value)
			seen[key] = true
			continue
		}
		out = append(out, item)
	}

	for _, key := range []string{"PORT", "HOST", "NUXT_PORT", "NUXT_HOST"} {
		if !seen[key] {
			out = append(out, key+"="+overrides[key])
		}
	}
	return out
}

func HasExplicitPortOrHostFlag(command string) bool {
	for _, field := range strings.Fields(command) {
		switch field {
		case "--port", "-p", "--host", "--hostname":
			return true
		}
		if strings.HasPrefix(field, "--port=") ||
			strings.HasPrefix(field, "--host=") ||
			strings.HasPrefix(field, "--hostname=") {
			return true
		}
	}
	return false
}

func InjectPortArgs(command string, port int, portFlag string) []string {
	return InjectPortArgsForHost(command, port, portFlag, "127.0.0.1")
}

func InjectPortArgsForHost(command string, port int, portFlag string, host string) []string {
	if host == "" {
		host = "127.0.0.1"
	}
	if HasExplicitPortOrHostFlag(command) {
		return nil
	}

	portValue := strconv.Itoa(port)
	if portFlag != "" {
		return []string{"--", portFlag, portValue}
	}

	tool := firstCommandWord(command)
	switch {
	case tool == "vite" || strings.Contains(command, "svelte-kit") || tool == "astro" || strings.HasPrefix(tool, "vp"):
		// Vite-like tools get --strictPort because gohere already selected a free port.
		return []string{"--", "--host", host, "--port", portValue, "--strictPort"}
	case tool == "next":
		return []string{"--", "-p", portValue}
	case tool == "nuxt":
		return []string{"--", "--host", host, "--port", portValue}
	case tool == "wrangler":
		return []string{"--", "--port", portValue}
	default:
		return nil
	}
}

func BuildScriptCommand(pm project.PackageManager, script string, extraArgs []string) []string {
	var cmd []string
	switch pm {
	case project.PackageManagerPNPM:
		cmd = []string{"pnpm", "run", script}
		extraArgs = trimArgSeparator(extraArgs)
	case project.PackageManagerYarn:
		cmd = []string{"yarn", script}
		extraArgs = trimArgSeparator(extraArgs)
	case project.PackageManagerBun:
		cmd = []string{"bun", "run", script}
		extraArgs = trimArgSeparator(extraArgs)
	default:
		cmd = []string{"npm", "run", script}
	}
	return append(cmd, extraArgs...)
}

func trimArgSeparator(args []string) []string {
	if len(args) > 0 && args[0] == "--" {
		return args[1:]
	}
	return args
}

func Start(ctx context.Context, cfg Config) (*Result, error) {
	if len(cfg.Command) == 0 {
		return nil, errors.New("command is required")
	}
	timeout := cfg.StartupTimeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	if cfg.Stdout == nil {
		cfg.Stdout = io.Discard
	}
	if cfg.Stderr == nil {
		cfg.Stderr = io.Discard
	}

	cmd := exec.CommandContext(ctx, cfg.Command[0], cfg.Command[1:]...)
	cmd.Env = cfg.Env
	if len(cmd.Env) == 0 {
		cmd.Env = os.Environ()
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	detected := make(chan int, 1)
	var scanWG sync.WaitGroup
	scanWG.Add(2)
	go streamAndScan(stdout, cfg.Stdout, detected, &scanWG)
	go streamAndScan(stderr, cfg.Stderr, detected, &scanWG)

	done := make(chan struct{})
	result := &Result{ctx: ctx, cmd: cmd, done: done}
	go func() {
		result.waitErr = cmd.Wait()
		scanWG.Wait()
		close(done)
	}()

	select {
	case port := <-detected:
		result.Port = port
		return result, nil
	case <-done:
		if result.waitErr == nil {
			result.waitErr = errors.New("process exited before a local URL was detected")
		}
		return nil, result.waitErr
	case <-time.After(timeout):
		if !cfg.RequireDetectedPort && cfg.ChosenPort != 0 && PortReachable(cfg.ChosenPort) {
			result.Port = cfg.ChosenPort
			return result, nil
		}
		cmd.Process.Kill()
		<-done
		return nil, errors.New("started dev script, but could not detect a local URL; try: gohere --target 5173")
	}
}

func streamAndScan(r io.Reader, w io.Writer, detected chan<- int, wg *sync.WaitGroup) {
	defer wg.Done()

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		io.WriteString(w, line+"\n")
		if port, ok := DetectPortFromOutput(line); ok {
			select {
			case detected <- port:
			default:
			}
		}
	}
}

func PortReachable(port int) bool {
	client := http.Client{Timeout: 200 * time.Millisecond}
	resp, err := client.Get("http://127.0.0.1:" + strconv.Itoa(port))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

func DetectPortFromOutput(line string) (int, bool) {
	matches := localURLPort.FindStringSubmatch(line)
	if len(matches) != 2 {
		return 0, false
	}
	port, err := strconv.Atoi(matches[1])
	if err != nil || port <= 0 || port > 65535 {
		return 0, false
	}
	return port, true
}

func firstCommandWord(command string) string {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return ""
	}
	word := fields[0]
	if slash := strings.LastIndex(word, "/"); slash >= 0 {
		word = word[slash+1:]
	}
	return strings.ToLower(word)
}

var localURLPort = regexp.MustCompile(`https?://(?:localhost|127\.0\.0\.1|0\.0\.0\.0):([0-9]{1,5})`)
