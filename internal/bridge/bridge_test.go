package bridge

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
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

func TestDiscoverWindowsTokenFindsSingleToken(t *testing.T) {
	root := t.TempDir()
	tokenPath := filepath.Join(root, "Jessa", ".gohere", "token")
	writeFile(t, tokenPath, "abc123\n")

	token, path, err := DiscoverWindowsToken(root)
	if err != nil {
		t.Fatal(err)
	}
	if token != "abc123" || path != tokenPath {
		t.Fatalf("token=%q path=%q", token, path)
	}
}

func TestDiscoverWindowsTokenReportsNoToken(t *testing.T) {
	_, _, err := DiscoverWindowsToken(t.TempDir())
	if !errors.Is(err, ErrWindowsTokenNotFound) {
		t.Fatalf("err = %v, want ErrWindowsTokenNotFound", err)
	}
}

func TestDiscoverWindowsTokenReportsMultipleTokens(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "Jessa", ".gohere", "token"), "one\n")
	writeFile(t, filepath.Join(root, "Roie", ".gohere", "token"), "two\n")

	_, _, err := DiscoverWindowsToken(root)
	if !errors.Is(err, ErrMultipleWindowsTokens) {
		t.Fatalf("err = %v, want ErrMultipleWindowsTokens", err)
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

func TestProbeServerListensOnAllInterfaces(t *testing.T) {
	probe, err := StartProbeServer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()

	host, _, err := net.SplitHostPort(probe.Addr)
	if err != nil {
		t.Fatal(err)
	}
	if host != "0.0.0.0" && host != "" {
		t.Fatalf("probe addr = %q, want wildcard", probe.Addr)
	}
}

func TestProbeBridgeAsksWindowsRouterToReachWSL(t *testing.T) {
	client := &fakeProbeClient{reachable: true}

	ok, target, err := ProbeBridge(context.Background(), client, "172.20.10.2")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected bridge to be reachable")
	}
	if client.target != target {
		t.Fatalf("probe target = %q, returned target %q", client.target, target)
	}
}

func TestProbeBridgeReportsUnreachable(t *testing.T) {
	client := &fakeProbeClient{reachable: false}

	ok, _, err := ProbeBridge(context.Background(), client, "172.20.10.2")
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

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}
