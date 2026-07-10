package bridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
)

var (
	ErrWSLIPNotFound = errors.New("wsl ipv4 address not found")
)

type ProbeClient interface {
	ProbeTarget(context.Context, string) (bool, error)
}

type ProbeServer struct {
	Addr string

	server *http.Server
	ln     net.Listener
	token  string
}

func IsWSLVersion(version string) bool {
	version = strings.ToLower(version)
	return strings.Contains(version, "microsoft") || strings.Contains(version, "wsl")
}

func IsWSLEnvironment(goos, distroName, interop, version string) bool {
	if goos != "linux" {
		return false
	}
	if distroName != "" || interop != "" {
		return true
	}
	return IsWSLVersion(version)
}

func DetectWSL() bool {
	data, err := os.ReadFile("/proc/version")
	version := ""
	if err == nil {
		version = string(data)
	}
	return IsWSLEnvironment(runtime.GOOS, os.Getenv("WSL_DISTRO_NAME"), os.Getenv("WSL_INTEROP"), version)
}

func IsWSL2KernelRelease(release string) bool {
	release = strings.ToLower(strings.TrimSpace(release))
	return strings.Contains(release, "wsl2") || strings.Contains(release, "microsoft-standard")
}

func DetectWSL2() bool {
	data, err := os.ReadFile("/proc/sys/kernel/osrelease")
	return err == nil && IsWSL2KernelRelease(string(data))
}

func FirstIPv4(output string) (string, error) {
	re := regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	for _, candidate := range re.FindAllString(output, -1) {
		ip := net.ParseIP(candidate)
		if ip == nil || ip.To4() == nil {
			continue
		}
		if ip.IsLoopback() {
			continue
		}
		return candidate, nil
	}
	return "", ErrWSLIPNotFound
}

func CurrentWSLIP(ctx context.Context) (string, error) {
	output, err := exec.CommandContext(ctx, "hostname", "-I").Output()
	if err != nil {
		return "", err
	}
	return FirstIPv4(string(output))
}

func StartProbeServer(ctx context.Context, host string) (*ProbeServer, error) {
	bindHost := probeBindHost(host)
	ln, err := net.Listen("tcp4", net.JoinHostPort(bindHost, "0"))
	if err != nil {
		return nil, err
	}
	token, err := probeToken()
	if err != nil {
		ln.Close()
		return nil, err
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != token {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			w.Write([]byte("ok\n"))
		}
	})}
	probe := &ProbeServer{Addr: ln.Addr().String(), server: server, ln: ln, token: token}
	go server.Serve(ln)
	go func() {
		<-ctx.Done()
		probe.Close()
	}()
	return probe, nil
}

func probeBindHost(host string) string {
	if host == "" || strings.EqualFold(host, "localhost") {
		return "127.0.0.1"
	}
	return host
}

func probeToken() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(token[:]), nil
}

func (s *ProbeServer) Target(host string) (string, error) {
	if s == nil {
		return "", errors.New("probe server is nil")
	}
	_, port, err := net.SplitHostPort(s.Addr)
	if err != nil {
		return "", err
	}
	return "http://" + net.JoinHostPort(host, port) + "/?token=" + s.token, nil
}

func (s *ProbeServer) Close() error {
	if s == nil {
		return nil
	}
	return s.server.Close()
}

func ProbeBridge(ctx context.Context, client ProbeClient, wslIP string) (bool, string, error) {
	probe, err := StartProbeServer(ctx, wslIP)
	if err != nil {
		return false, "", err
	}
	defer probe.Close()

	target, err := probe.Target(wslIP)
	if err != nil {
		return false, "", err
	}
	reachable, err := client.ProbeTarget(ctx, target)
	if err != nil {
		return false, target, err
	}
	return reachable, target, nil
}
