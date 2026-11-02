package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/roie/gohere/internal/router"
)

type fakeRouterProcess struct {
	before       []processState
	after        []processState
	inspectErr   error
	terminateErr error
	onTerminate  func()
	terminated   int
	inspections  int
	closed       bool
}

func (p *fakeRouterProcess) Inspect(string) (processState, error) {
	p.inspections++
	states := &p.before
	if p.terminated > 0 && len(p.after) > 0 {
		states = &p.after
	}
	state := processMatched
	if len(*states) > 0 {
		state = (*states)[0]
		if len(*states) > 1 {
			*states = (*states)[1:]
		}
	}
	if state == processUnknown {
		return state, cmpErr(p.inspectErr, errors.New("inspection failed"))
	}
	return state, nil
}

func cmpErr(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}

func (p *fakeRouterProcess) Terminate() error {
	p.terminated++
	if p.onTerminate != nil {
		p.onTerminate()
	}
	return p.terminateErr
}

func (p *fakeRouterProcess) Close() error {
	p.closed = true
	return nil
}

type stopHarness struct {
	stateDir     string
	configDir    string
	runner       *uninstallRecordingRunner
	process      *fakeRouterProcess
	openedPID    int
	sleeps       int
	adminCalls   int
	sleepHook    func(int)
	onShutdown   func()
	shutdownErr  error
	healthErr    error
	withPID      string
	systemdUnit  bool
	systemdError error
}

func newStopHarness(t *testing.T) *stopHarness {
	t.Helper()
	h := &stopHarness{
		stateDir:  t.TempDir(),
		configDir: t.TempDir(),
		process:   &fakeRouterProcess{},
		healthErr: errors.New("connection refused"),
		withPID:   "4242\n",
	}
	oldShutdown, oldHealth := adminShutdown, adminHealth
	t.Cleanup(func() { adminShutdown, adminHealth = oldShutdown, oldHealth })
	adminShutdown = func(context.Context, string) error {
		h.adminCalls++
		if h.onShutdown != nil {
			h.onShutdown()
		}
		return h.shutdownErr
	}
	adminHealth = func(context.Context) error { return h.healthErr }
	return h
}

func (h *stopHarness) run(t *testing.T, ctx context.Context) (string, error) {
	t.Helper()
	writeFile(t, filepath.Join(h.stateDir, "token"), strings.Repeat("a", 64)+"\n")
	if h.withPID != "" {
		writeFile(t, filepath.Join(h.stateDir, router.RouterPIDFilename), h.withPID)
	}
	if h.systemdUnit {
		writeFile(t, filepath.Join(h.configDir, "systemd", "user", "gohere-router.service"), "service")
	}
	h.runner = &uninstallRecordingRunner{err: h.systemdError}
	var out strings.Builder
	err := ServiceStopWithConfig(ctx, &out, ServiceStopConfig{
		StateDir:      h.stateDir,
		ConfigDir:     h.configDir,
		CommandRunner: h.runner,
		OpenProcess: func(pid int) routerProcess {
			h.openedPID = pid
			return h.process
		},
		Sleep: func(ctx context.Context, d time.Duration) error {
			if d != routerStopPoll {
				t.Fatalf("sleep = %v, want %v", d, routerStopPoll)
			}
			h.sleeps++
			if h.sleepHook != nil {
				h.sleepHook(h.sleeps)
			}
			return ctx.Err()
		},
	})
	return out.String(), err
}

func (h *stopHarness) pidRecord(t *testing.T) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h.stateDir, router.RouterPIDFilename))
	if errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data), true
}

func assertStopped(t *testing.T, h *stopHarness, out string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("stop error = %v", err)
	}
	if out != "gohere service stopped.\n" {
		t.Fatalf("output = %q", out)
	}
	if record, ok := h.pidRecord(t); ok {
		t.Fatalf("router.pid = %q, want removed", record)
	}
}

func assertStopFailedPreservingPID(t *testing.T, h *stopHarness, out string, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("stop error = %v, want %q", err, want)
	}
	if out != "" {
		t.Fatalf("output = %q, want none", out)
	}
	if record, ok := h.pidRecord(t); !ok || record != h.withPID {
		t.Fatalf("router.pid = %q (present %v), want preserved %q", record, ok, h.withPID)
	}
}

func TestConfirmedStopWaitsForGracefulExit(t *testing.T) {
	h := newStopHarness(t)
	h.process.before = []processState{processMatched, processMatched, processMatched, processAbsent}

	out, err := h.run(t, t.Context())

	assertStopped(t, h, out, err)
	if h.openedPID != 4242 || h.adminCalls != 1 || h.process.terminated != 0 || !h.process.closed {
		t.Fatalf("opened %d, admin calls %d, terminated %d, closed %v", h.openedPID, h.adminCalls, h.process.terminated, h.process.closed)
	}
	if h.sleeps != 3 {
		t.Fatalf("sleeps = %d, want 3 polls before exit", h.sleeps)
	}
}

func TestConfirmedStopConfirmsRouterThatExitsDuringShutdownRequest(t *testing.T) {
	h := newStopHarness(t)
	h.onShutdown = func() {
		h.process.before = []processState{processAbsent}
		if err := os.Remove(filepath.Join(h.stateDir, router.RouterPIDFilename)); err != nil {
			t.Fatal(err)
		}
	}

	out, err := h.run(t, t.Context())

	assertStopped(t, h, out, err)
	if h.process.terminated != 0 {
		t.Fatalf("terminated = %d, want 0", h.process.terminated)
	}
}

func TestConfirmedStopSignalsOnceAfterFullGracePeriod(t *testing.T) {
	h := newStopHarness(t)
	h.process.after = []processState{processMatched, processMatched, processMismatched}

	out, err := h.run(t, t.Context())

	assertStopped(t, h, out, err)
	if h.process.terminated != 1 {
		t.Fatalf("terminated = %d, want 1", h.process.terminated)
	}
	if want := int(routerGracefulStopWait/routerStopPoll) + 3; h.sleeps != want {
		t.Fatalf("sleeps = %d, want %d", h.sleeps, want)
	}
}

func TestConfirmedStopSignalsImmediatelyWithoutGracefulShutdown(t *testing.T) {
	h := newStopHarness(t)
	h.shutdownErr = errors.New("connection refused")
	h.process.after = []processState{processAbsent}

	out, err := h.run(t, t.Context())

	assertStopped(t, h, out, err)
	if h.process.terminated != 1 || h.sleeps != 1 {
		t.Fatalf("terminated %d, sleeps %d; want immediate signal then one poll", h.process.terminated, h.sleeps)
	}
}

func TestConfirmedStopUsesSystemdAsGracefulShutdown(t *testing.T) {
	h := newStopHarness(t)
	h.shutdownErr = errors.New("connection refused")
	h.systemdUnit = true
	h.process.before = []processState{processMatched, processAbsent}

	out, err := h.run(t, t.Context())

	assertStopped(t, h, out, err)
	if h.process.terminated != 0 || !h.runner.saw("systemctl", "--user", "stop", "gohere-router") {
		t.Fatalf("terminated %d, commands %#v", h.process.terminated, h.runner.commands)
	}
}

func TestConfirmedStopAcceptsConcurrentExitAfterSignalError(t *testing.T) {
	h := newStopHarness(t)
	h.shutdownErr = errors.New("connection refused")
	h.process.terminateErr = errors.New("no such process")
	h.process.after = []processState{processAbsent}

	out, err := h.run(t, t.Context())

	assertStopped(t, h, out, err)
}

func TestConfirmedStopFailures(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*stopHarness)
		want  string
		admin int
		term  int
	}{
		{
			name: "signal error while still matched",
			setup: func(h *stopHarness) {
				h.shutdownErr = errors.New("down")
				h.process.terminateErr = errors.New("permission denied")
			},
			want: "unable to signal router process 4242", admin: 1, term: 1,
		},
		{
			name: "still matched after signal",
			setup: func(h *stopHarness) {
				h.shutdownErr = errors.New("down")
			},
			want: "timed out waiting for router process 4242", admin: 1, term: 1,
		},
		{
			name: "unknown before shutdown",
			setup: func(h *stopHarness) {
				h.process.before = []processState{processUnknown}
				h.process.inspectErr = errors.New("sysctl failed")
			},
			want: "sysctl failed", admin: 0, term: 0,
		},
		{
			name: "unknown while waiting",
			setup: func(h *stopHarness) {
				h.process.before = []processState{processMatched, processMatched, processUnknown}
			},
			want: "unable to verify router process", admin: 1, term: 0,
		},
		{
			name: "invalid record",
			setup: func(h *stopHarness) {
				h.withPID = "not-a-pid\n"
			},
			want: "invalid router PID record", admin: 0, term: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newStopHarness(t)
			tt.setup(h)

			out, err := h.run(t, t.Context())

			assertStopFailedPreservingPID(t, h, out, err, tt.want)
			if h.adminCalls != tt.admin || h.process.terminated != tt.term {
				t.Fatalf("admin calls %d, terminated %d; want %d, %d", h.adminCalls, h.process.terminated, tt.admin, tt.term)
			}
		})
	}
}

func TestConfirmedStopCancellationNeverSignals(t *testing.T) {
	h := newStopHarness(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h.sleepHook = func(int) { cancel() }

	out, err := h.run(t, ctx)

	assertStopFailedPreservingPID(t, h, out, err, "context canceled")
	if !errors.Is(err, context.Canceled) || h.process.terminated != 0 {
		t.Fatalf("err %v, terminated %d", err, h.process.terminated)
	}
}

func TestConfirmedStopReportsReplacedPIDRecord(t *testing.T) {
	h := newStopHarness(t)
	h.shutdownErr = errors.New("down")
	h.process.after = []processState{processAbsent}
	h.process.onTerminate = func() {
		writeFile(t, filepath.Join(h.stateDir, router.RouterPIDFilename), "5555\n")
	}

	_, err := h.run(t, t.Context())

	if !errors.Is(err, router.ErrRuntimeRecordChanged) {
		t.Fatalf("err = %v, want ErrRuntimeRecordChanged", err)
	}
	if record, _ := h.pidRecord(t); record != "5555\n" {
		t.Fatalf("router.pid = %q, want replacement preserved", record)
	}
}

func TestConfirmedStopSystemdFailureIsNotHiddenByConfirmedExit(t *testing.T) {
	h := newStopHarness(t)
	h.systemdUnit = true
	h.systemdError = errors.New("unit busy")
	h.process.before = []processState{processMatched, processAbsent}

	_, err := h.run(t, t.Context())

	if err == nil || !strings.Contains(err.Error(), "unit busy") {
		t.Fatalf("err = %v, want systemd failure", err)
	}
	if _, ok := h.pidRecord(t); ok {
		t.Fatal("router.pid should be removed after confirmed exit")
	}
}

func TestConfirmedStopWithoutLiveRecordedProcess(t *testing.T) {
	tests := []struct {
		name        string
		pid         string
		state       processState
		shutdownErr error
		healthErr   error
		systemd     bool
		wantOut     string
		wantErr     string
	}{
		{name: "nothing running", shutdownErr: errors.New("down"), healthErr: errors.New("down"), wantOut: "No gohere service is running.\n"},
		{name: "stale pid nothing running", pid: "4242\n", state: processMismatched, shutdownErr: errors.New("down"), healthErr: errors.New("down"), wantOut: "No gohere service is running.\n"},
		{name: "absent pid nothing running", pid: "4242\n", state: processAbsent, shutdownErr: errors.New("down"), healthErr: errors.New("down"), wantOut: "No gohere service is running.\n"},
		{name: "accepted without pid", wantErr: "cannot be confirmed"},
		{name: "accepted with stale pid", pid: "4242\n", state: processAbsent, wantErr: "cannot be confirmed"},
		{name: "router still answering", shutdownErr: errors.New("401 unauthorized"), wantErr: "router is responding but could not be stopped"},
		{name: "systemd stop confirms", systemd: true, shutdownErr: errors.New("down"), wantOut: "gohere service stopped.\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newStopHarness(t)
			h.withPID = tt.pid
			h.process.before = []processState{tt.state}
			h.shutdownErr = tt.shutdownErr
			h.healthErr = tt.healthErr
			h.systemdUnit = tt.systemd

			out, err := h.run(t, t.Context())

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
			} else if err != nil || out != tt.wantOut {
				t.Fatalf("out %q, err %v; want %q", out, err, tt.wantOut)
			}
			if h.process.terminated != 0 {
				t.Fatalf("terminated %d, want 0", h.process.terminated)
			}
			if _, ok := h.pidRecord(t); ok {
				t.Fatal("stale router.pid should be removed")
			}
		})
	}
}
