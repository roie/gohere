package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/roie/gohere/internal/companion"
	"github.com/roie/gohere/internal/wsledge"
)

func TestEnsureWSLPublicTransportUsesVerifiedDirectPath(t *testing.T) {
	restore := stubWSLEdgeFunctions(t)
	defer restore()
	candidate := testEdgeCandidate(t, "candidate")
	currentWSLMetadataFunc = func() (wslIntegrationMetadata, error) { return candidate, nil }
	probeWSLDirectRouterIdentityFunc = identityProbeSequence(identityProbeResult{matched: true, observed: "router-1"})
	probeWSLRouterIdentityFunc = identityProbeSequence(identityProbeResult{matched: true, observed: "router-1"})
	inspectWSLEdgeFunc = func(string) (wsledge.RunningInfo, bool, error) { return wsledge.RunningInfo{}, false, nil }
	startWSLEdgeFunc = func(context.Context, string, string, string) (int, error) {
		t.Fatal("edge should not start for a verified direct path")
		return 0, nil
	}

	transport, err := ensureWSLPublicTransport(t.Context(), companion.Info{RouterInstanceID: "router-1"}, `C:\Temp\gohere.exe`)
	if err != nil || transport != wslTransportDirect {
		t.Fatalf("transport = %q, err = %v", transport, err)
	}
}

func TestEnsureWSLPublicTransportStopsVerifiedEdgeAfterDirectDetection(t *testing.T) {
	restore := stubWSLEdgeFunctions(t)
	defer restore()
	candidate := testEdgeCandidate(t, "candidate")
	currentWSLMetadataFunc = func() (wslIntegrationMetadata, error) { return candidate, nil }
	probeWSLDirectRouterIdentityFunc = identityProbeSequence(identityProbeResult{matched: true, observed: "router-1"})
	probeWSLRouterIdentityFunc = identityProbeSequence(identityProbeResult{matched: true, observed: "router-1"})
	previous := wsledge.RunningInfo{PID: 10, ProcessIdentity: "linux:1", EdgeBinary: "/old/edge", EdgeSHA256: strings.Repeat("1", 64), CompanionBinary: `C:\old.exe`}
	inspectWSLEdgeFunc = func(string) (wsledge.RunningInfo, bool, error) { return previous, true, nil }
	stopped := false
	wslStopEdgeFunc = func(string) error { stopped = true; return nil }

	transport, err := ensureWSLPublicTransport(t.Context(), companion.Info{RouterInstanceID: "router-1"}, `C:\Temp\gohere.exe`)
	if err != nil || transport != wslTransportDirect || !stopped {
		t.Fatalf("transport = %q, stopped = %v, err = %v", transport, stopped, err)
	}
}

func TestEnsureWSLPublicTransportRestoresEdgeAfterFalsePositiveDirectProbe(t *testing.T) {
	restore := stubWSLEdgeFunctions(t)
	defer restore()
	candidate := testEdgeCandidate(t, "candidate")
	currentWSLMetadataFunc = func() (wslIntegrationMetadata, error) { return candidate, nil }
	probeWSLDirectRouterIdentityFunc = identityProbeSequence(identityProbeResult{matched: true, observed: "router-1"})
	probeWSLRouterIdentityFunc = identityProbeSequence(
		identityProbeResult{err: errors.New("direct route unavailable")},
		identityProbeResult{matched: true, observed: "router-1"},
	)
	previous := wsledge.RunningInfo{PID: 10, ProcessIdentity: "linux:1", EdgeBinary: "/old/edge", EdgeSHA256: strings.Repeat("1", 64), CompanionBinary: `C:\old.exe`}
	inspectWSLEdgeFunc = func(string) (wsledge.RunningInfo, bool, error) { return previous, true, nil }
	wslStopEdgeFunc = func(string) error { return nil }
	var starts [][2]string
	startWSLEdgeFunc = func(_ context.Context, edge, companion, _ string) (int, error) {
		starts = append(starts, [2]string{edge, companion})
		return 11, nil
	}

	_, err := ensureWSLPublicTransport(t.Context(), companion.Info{RouterInstanceID: "router-1"}, `C:\Temp\gohere.exe`)
	if err == nil || !strings.Contains(err.Error(), "direct transport validation failed") {
		t.Fatalf("error = %v", err)
	}
	if len(starts) != 1 || starts[0] != [2]string{previous.EdgeBinary, previous.CompanionBinary} {
		t.Fatalf("rollback starts = %#v", starts)
	}
}

func TestEnsureWSLPublicTransportReusesMatchingVerifiedEdge(t *testing.T) {
	restore := stubWSLEdgeFunctions(t)
	defer restore()
	candidate := testEdgeCandidate(t, "candidate")
	currentWSLMetadataFunc = func() (wslIntegrationMetadata, error) { return candidate, nil }
	probeWSLDirectRouterIdentityFunc = identityProbeSequence(identityProbeResult{err: errors.New("NAT")})
	probeWSLRouterIdentityFunc = identityProbeSequence(identityProbeResult{matched: true, observed: "router-1"})
	running := wsledge.RunningInfo{PID: 10, ProcessIdentity: "linux:1", EdgeBinary: candidate.EdgeBinary, EdgeSHA256: candidate.EdgeSHA256, CompanionBinary: `C:\Temp\gohere.exe`}
	inspectWSLEdgeFunc = func(string) (wsledge.RunningInfo, bool, error) { return running, true, nil }
	wslStopEdgeFunc = func(string) error { t.Fatal("matching edge should not stop"); return nil }
	startWSLEdgeFunc = func(context.Context, string, string, string) (int, error) { t.Fatal("matching edge should not restart"); return 0, nil }

	transport, err := ensureWSLPublicTransport(t.Context(), companion.Info{RouterInstanceID: "router-1"}, `C:\Temp\gohere.exe`)
	if err != nil || transport != wslTransportEdge {
		t.Fatalf("transport = %q, err = %v", transport, err)
	}
}

func TestEnsureWSLPublicTransportReplacesMismatchedEdge(t *testing.T) {
	restore := stubWSLEdgeFunctions(t)
	defer restore()
	candidate := testEdgeCandidate(t, "candidate")
	currentWSLMetadataFunc = func() (wslIntegrationMetadata, error) { return candidate, nil }
	probeWSLDirectRouterIdentityFunc = identityProbeSequence(identityProbeResult{err: errors.New("NAT")})
	probeWSLRouterIdentityFunc = identityProbeSequence(identityProbeResult{matched: true, observed: "router-1"})
	previous := wsledge.RunningInfo{PID: 10, ProcessIdentity: "linux:1", EdgeBinary: "/old/edge", EdgeSHA256: strings.Repeat("1", 64), CompanionBinary: `C:\old.exe`}
	inspectWSLEdgeFunc = func(string) (wsledge.RunningInfo, bool, error) { return previous, true, nil }
	stopped := false
	wslStopEdgeFunc = func(string) error { stopped = true; return nil }
	var started [2]string
	startWSLEdgeFunc = func(_ context.Context, edge, companion, _ string) (int, error) {
		started = [2]string{edge, companion}
		return 11, nil
	}

	transport, err := ensureWSLPublicTransport(t.Context(), companion.Info{RouterInstanceID: "router-1"}, `C:\Temp\gohere.exe`)
	if err != nil || transport != wslTransportEdge || !stopped {
		t.Fatalf("transport = %q, stopped = %v, err = %v", transport, stopped, err)
	}
	if started != [2]string{candidate.EdgeBinary, `C:\Temp\gohere.exe`} {
		t.Fatalf("started = %#v", started)
	}
}

func TestEnsureWSLPublicTransportRollsBackFailedCandidate(t *testing.T) {
	restore := stubWSLEdgeFunctions(t)
	defer restore()
	candidate := testEdgeCandidate(t, "candidate")
	currentWSLMetadataFunc = func() (wslIntegrationMetadata, error) { return candidate, nil }
	probeWSLDirectRouterIdentityFunc = identityProbeSequence(identityProbeResult{err: errors.New("NAT")})
	previous := wsledge.RunningInfo{PID: 10, ProcessIdentity: "linux:1", EdgeBinary: "/old/edge", EdgeSHA256: strings.Repeat("1", 64), CompanionBinary: `C:\old.exe`}
	inspectWSLEdgeFunc = func(string) (wsledge.RunningInfo, bool, error) { return previous, true, nil }
	stops := 0
	wslStopEdgeFunc = func(string) error { stops++; return nil }
	var starts [][2]string
	startWSLEdgeFunc = func(_ context.Context, edge, companion, _ string) (int, error) {
		starts = append(starts, [2]string{edge, companion})
		return 11, nil
	}
	probeWSLRouterIdentityFunc = func(context.Context, string, string) (bool, string, error) {
		if len(starts) >= 2 {
			return true, "router-1", nil
		}
		return false, "", errors.New("candidate unavailable")
	}
	wslEdgeReadyTimeout = 10 * time.Millisecond

	_, err := ensureWSLPublicTransport(t.Context(), companion.Info{RouterInstanceID: "router-1"}, `C:\Temp\gohere.exe`)
	if err == nil || !strings.Contains(err.Error(), "replacement failed") {
		t.Fatalf("error = %v", err)
	}
	if len(starts) != 2 || starts[1] != [2]string{previous.EdgeBinary, previous.CompanionBinary} {
		t.Fatalf("starts = %#v", starts)
	}
	if stops != 2 {
		t.Fatalf("edge stops = %d, want old and failed candidate stopped", stops)
	}
}

func TestEnsureWSLPublicTransportReportsReplacementAndRollbackFailure(t *testing.T) {
	restore := stubWSLEdgeFunctions(t)
	defer restore()
	candidate := testEdgeCandidate(t, "candidate")
	currentWSLMetadataFunc = func() (wslIntegrationMetadata, error) { return candidate, nil }
	probeWSLDirectRouterIdentityFunc = identityProbeSequence(identityProbeResult{err: errors.New("NAT")})
	probeWSLRouterIdentityFunc = identityProbeSequence(identityProbeResult{err: errors.New("identity unavailable")})
	previous := wsledge.RunningInfo{PID: 10, ProcessIdentity: "linux:1", EdgeBinary: "/old/edge", EdgeSHA256: strings.Repeat("1", 64), CompanionBinary: `C:\old.exe`}
	inspectWSLEdgeFunc = func(string) (wsledge.RunningInfo, bool, error) { return previous, true, nil }
	wslStopEdgeFunc = func(string) error { return nil }
	startWSLEdgeFunc = func(_ context.Context, edge, _, _ string) (int, error) {
		if edge == previous.EdgeBinary {
			return 0, errors.New("rollback start failed")
		}
		return 11, nil
	}
	wslEdgeReadyTimeout = 10 * time.Millisecond

	_, err := ensureWSLPublicTransport(t.Context(), companion.Info{RouterInstanceID: "router-1"}, `C:\Temp\gohere.exe`)
	if err == nil || !strings.Contains(err.Error(), "identity unavailable") || !strings.Contains(err.Error(), "rollback start failed") {
		t.Fatalf("error = %v", err)
	}
}

type identityProbeResult struct {
	matched  bool
	observed string
	err      error
}

func identityProbeSequence(results ...identityProbeResult) func(context.Context, string, string) (bool, string, error) {
	index := 0
	return func(context.Context, string, string) (bool, string, error) {
		if len(results) == 0 {
			return false, "", errors.New("no probe result")
		}
		if index >= len(results) {
			return results[len(results)-1].matched, results[len(results)-1].observed, results[len(results)-1].err
		}
		result := results[index]
		index++
		return result.matched, result.observed, result.err
	}
}

func testEdgeCandidate(t *testing.T, contents string) wslIntegrationMetadata {
	t.Helper()
	path := filepath.Join(t.TempDir(), "edge")
	if err := os.WriteFile(path, []byte(contents), 0755); err != nil {
		t.Fatal(err)
	}
	hash, err := fileSHA256Hex(path)
	if err != nil {
		t.Fatal(err)
	}
	return wslIntegrationMetadata{EdgeBinary: path, EdgeSHA256: hash, CompanionVersion: "1.2.3"}
}

func stubWSLEdgeFunctions(t *testing.T) func() {
	t.Helper()
	oldPublicProbe := probeWSLRouterIdentityFunc
	oldDirectProbe := probeWSLDirectRouterIdentityFunc
	oldStart := startWSLEdgeFunc
	oldInspect := inspectWSLEdgeFunc
	oldStop := wslStopEdgeFunc
	oldMetadata := currentWSLMetadataFunc
	oldTimeout := wslEdgeReadyTimeout
	return func() {
		probeWSLRouterIdentityFunc = oldPublicProbe
		probeWSLDirectRouterIdentityFunc = oldDirectProbe
		startWSLEdgeFunc = oldStart
		inspectWSLEdgeFunc = oldInspect
		wslStopEdgeFunc = oldStop
		currentWSLMetadataFunc = oldMetadata
		wslEdgeReadyTimeout = oldTimeout
	}
}
