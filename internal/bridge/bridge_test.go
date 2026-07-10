package bridge

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
)

func TestIsWSLVersion(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    bool
	}{
		{"microsoft kernel", "Linux version 5.15.167.4-microsoft-standard-WSL2", true},
		{"wsl lowercase", "linux version 6.6.87.2-microsoft-standard-wsl2", true},
		{"normal linux", "Linux version 6.8.0-generic", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsWSLVersion(tt.version); got != tt.want {
				t.Fatalf("IsWSLVersion(%q) = %v, want %v", tt.version, got, tt.want)
			}
		})
	}
}

func TestIsWSLEnvironmentOnlyAllowsLinux(t *testing.T) {
	if IsWSLEnvironment("windows", "Ubuntu", "/run/WSL/1_interop", "Linux version microsoft") {
		t.Fatal("windows build should not detect WSL even with WSL environment variables")
	}
	if IsWSLEnvironment("darwin", "Ubuntu", "/run/WSL/1_interop", "Linux version microsoft") {
		t.Fatal("darwin build should not detect WSL even with WSL environment variables")
	}
	if !IsWSLEnvironment("linux", "Ubuntu", "", "") {
		t.Fatal("linux build should detect WSL distro environment")
	}
}

func TestIsWSL2KernelRelease(t *testing.T) {
	for _, release := range []string{
		"6.6.87.2-microsoft-standard-WSL2",
		"4.19.128-microsoft-standard",
	} {
		if !IsWSL2KernelRelease(release) {
			t.Fatalf("release %q was not recognized as WSL2", release)
		}
	}
	for _, release := range []string{"4.4.0-19041-Microsoft", "6.8.0-generic"} {
		if IsWSL2KernelRelease(release) {
			t.Fatalf("release %q was incorrectly recognized as WSL2", release)
		}
	}
}

func TestFirstIPv4(t *testing.T) {
	got, err := FirstIPv4("172.20.10.2 10.255.255.1 fe80::1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "172.20.10.2" {
		t.Fatalf("ip = %q", got)
	}
}

func TestFirstIPv4ReportsMissingIP(t *testing.T) {
	_, err := FirstIPv4("fe80::1")
	if !errors.Is(err, ErrWSLIPNotFound) {
		t.Fatalf("err = %v, want ErrWSLIPNotFound", err)
	}
}

func TestProbeServerListensOnRequestedHost(t *testing.T) {
	probe, err := StartProbeServer(context.Background(), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()

	host, _, err := net.SplitHostPort(probe.Addr)
	if err != nil {
		t.Fatal(err)
	}
	if host != "127.0.0.1" {
		t.Fatalf("probe addr = %q, want 127.0.0.1", probe.Addr)
	}
}

func TestProbeServerRequiresToken(t *testing.T) {
	probe, err := StartProbeServer(context.Background(), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()

	target, err := probe.Target("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(target, "token=") {
		t.Fatalf("target = %q, want token query", target)
	}
	req, err := http.NewRequest(http.MethodHead, "http://"+probe.Addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("probe server accepted request without token")
	}

	req, err = http.NewRequest(http.MethodHead, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authorized probe status = %d", resp.StatusCode)
	}
}

func TestProbeBridgeAsksWindowsRouterToReachWSL(t *testing.T) {
	client := &fakeProbeClient{reachable: true}

	ok, target, err := ProbeBridge(context.Background(), client, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected bridge to be reachable")
	}
	if client.target != target {
		t.Fatalf("probe target = %q, returned target %q", client.target, target)
	}
	if !strings.Contains(target, "token=") {
		t.Fatalf("probe target = %q, want token query", target)
	}
}

func TestProbeBridgeReportsUnreachable(t *testing.T) {
	client := &fakeProbeClient{reachable: false}

	ok, _, err := ProbeBridge(context.Background(), client, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected bridge to be unreachable")
	}
}

type fakeProbeClient struct {
	reachable bool
	target    string
}

func (c *fakeProbeClient) ProbeTarget(ctx context.Context, target string) (bool, error) {
	c.target = target
	return c.reachable, nil
}
