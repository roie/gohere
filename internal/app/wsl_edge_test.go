package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/roie/gohere/internal/companion"
	"github.com/roie/gohere/internal/router"
)

func TestEnsureWSLPublicTransportUsesVerifiedDirectPath(t *testing.T) {
	restore := stubWSLEdgeFunctions(t)
	defer restore()
	probeWSLRouterIdentityFunc = func(context.Context, string, string) (bool, string, error) {
		return true, "router-1", nil
	}
	wslEdgeRunningFunc = func(string) bool { return false }
	startWSLEdgeFunc = func(context.Context, string, string, string) (int, error) {
		t.Fatal("edge should not start for a verified direct path")
		return 0, nil
	}

	transport, err := ensureWSLPublicTransport(t.Context(), companion.Info{RouterInstanceID: "router-1"}, `C:\Temp\gohere.exe`)
	if err != nil || transport != wslTransportDirect {
		t.Fatalf("transport = %q, err = %v", transport, err)
	}
}

func TestEnsureWSLPublicTransportRecognizesExistingEdge(t *testing.T) {
	restore := stubWSLEdgeFunctions(t)
	defer restore()
	probeWSLRouterIdentityFunc = func(context.Context, string, string) (bool, string, error) {
		return true, "router-1", nil
	}
	wslEdgeRunningFunc = func(string) bool { return true }

	transport, err := ensureWSLPublicTransport(t.Context(), companion.Info{RouterInstanceID: "router-1"}, `C:\Temp\gohere.exe`)
	if err != nil || transport != wslTransportEdge {
		t.Fatalf("transport = %q, err = %v", transport, err)
	}
}

func TestEnsureWSLPublicTransportStartsEdgeWhenLoopbacksAreSeparate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	restore := stubWSLEdgeFunctions(t)
	defer restore()
	integrationDir := filepath.Join(router.DefaultStateDir(), wslIntegrationDirname)
	edgeBinary := filepath.Join(integrationDir, "bin", wslEdgeBinaryName)
	if err := os.MkdirAll(filepath.Dir(edgeBinary), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(edgeBinary, []byte("edge"), 0755); err != nil {
		t.Fatal(err)
	}
	probes := 0
	probeWSLRouterIdentityFunc = func(context.Context, string, string) (bool, string, error) {
		probes++
		if probes >= 2 {
			return true, "router-1", nil
		}
		return false, "", errors.New("connection refused")
	}
	wslEdgeRunningFunc = func(string) bool { return false }
	started := false
	startWSLEdgeFunc = func(_ context.Context, gotEdge, gotCompanion, gotLog string) (int, error) {
		started = true
		if gotEdge != edgeBinary || gotCompanion != `C:\Temp\gohere.exe` || gotLog != filepath.Join(integrationDir, "edge.log") {
			t.Fatalf("start args = %q, %q, %q", gotEdge, gotCompanion, gotLog)
		}
		return 123, nil
	}

	transport, err := ensureWSLPublicTransport(t.Context(), companion.Info{RouterInstanceID: "router-1"}, `C:\Temp\gohere.exe`)
	if err != nil || transport != wslTransportEdge || !started {
		t.Fatalf("transport = %q, started = %v, err = %v", transport, started, err)
	}
}

func stubWSLEdgeFunctions(t *testing.T) func() {
	t.Helper()
	oldProbe := probeWSLRouterIdentityFunc
	oldStart := startWSLEdgeFunc
	oldRunning := wslEdgeRunningFunc
	return func() {
		probeWSLRouterIdentityFunc = oldProbe
		startWSLEdgeFunc = oldStart
		wslEdgeRunningFunc = oldRunning
	}
}
