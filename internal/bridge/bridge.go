package bridge

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	ErrWindowsTokenNotFound  = errors.New("windows gohere token not found")
	ErrMultipleWindowsTokens = errors.New("multiple windows gohere tokens found")
	ErrWSLIPNotFound         = errors.New("wsl ipv4 address not found")
)

type ProbeClient interface {
	ProbeTarget(context.Context, string) (bool, error)
}

type ProbeServer struct {
	Addr string

	server *http.Server
	ln     net.Listener
}

func IsWSLVersion(version string) bool {
	version = strings.ToLower(version)
	return strings.Contains(version, "microsoft") || strings.Contains(version, "wsl")
}

func DetectWSL() bool {
	if os.Getenv("WSL_DISTRO_NAME") != "" || os.Getenv("WSL_INTEROP") != "" {
		return true
	}
	data, err := os.ReadFile("/proc/version")
	if err != nil {
		return false
	}
	return IsWSLVersion(string(data))
}

func DiscoverWindowsToken(usersRoot string) (string, string, error) {
	matches, err := filepath.Glob(filepath.Join(usersRoot, "*", ".gohere", "token"))
	if err != nil {
		return "", "", err
	}
	switch len(matches) {
	case 0:
		return "", "", ErrWindowsTokenNotFound
	case 1:
		data, err := os.ReadFile(matches[0])
		if err != nil {
			return "", "", err
		}
		return strings.TrimSpace(string(data)), matches[0], nil
	default:
		return "", "", fmt.Errorf("%w: %s", ErrMultipleWindowsTokens, strings.Join(matches, ", "))
	}
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

func StartProbeServer(ctx context.Context) (*ProbeServer, error) {
	ln, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		return nil, err
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok\n"))
	})}
	probe := &ProbeServer{Addr: ln.Addr().String(), server: server, ln: ln}
	go server.Serve(ln)
	go func() {
		<-ctx.Done()
		probe.Close()
	}()
	return probe, nil
}

func (s *ProbeServer) Close() error {
	if s == nil {
		return nil
	}
	return s.server.Close()
}

func ProbeBridge(ctx context.Context, client ProbeClient, wslIP string) (bool, string, error) {
	probe, err := StartProbeServer(ctx)
	if err != nil {
		return false, "", err
	}
	defer probe.Close()

	_, port, err := net.SplitHostPort(probe.Addr)
	if err != nil {
		return false, "", err
	}
	target := "http://" + net.JoinHostPort(wslIP, port)
	reachable, err := client.ProbeTarget(ctx, target)
	if err != nil {
		return false, target, err
	}
	return reachable, target, nil
}
