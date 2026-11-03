package app

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	localcert "github.com/roie/gohere/internal/cert"
	appconfig "github.com/roie/gohere/internal/config"
	"github.com/roie/gohere/internal/router"
)

type uninstallOutcome struct {
	err          error
	out          string
	untrusted    bool
	pathRemoved  bool
	firewallRun  bool
	stateRemoved bool
}

func (h *stopHarness) runUninstall(t *testing.T, ctx context.Context) uninstallOutcome {
	t.Helper()
	writeFile(t, filepath.Join(h.stateDir, "token"), strings.Repeat("a", 64)+"\n")
	writeFile(t, filepath.Join(h.stateDir, "bin", "gohere"), "binary")
	writeFile(t, filepath.Join(h.stateDir, "bin", "gohere.exe"), "binary")
	writeFile(t, filepath.Join(h.stateDir, "routes.json"), "[]")
	if err := appconfig.Save(h.stateDir, appconfig.Config{HTTPS: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := (localcert.Store{StateDir: h.stateDir}).EnsureCA(); err != nil {
		t.Fatal(err)
	}
	if h.withPID != "" {
		writeFile(t, filepath.Join(h.stateDir, router.RouterPIDFilename), h.withPID)
	}
	if h.systemdUnit {
		writeFile(t, filepath.Join(h.configDir, "systemd", "user", "gohere-router.service"), "service")
	}
	h.runner = &uninstallRecordingRunner{err: h.systemdError}
	removeState := true
	var outcome uninstallOutcome
	var out strings.Builder
	outcome.err = UninstallWithConfig(ctx, &out, UninstallConfig{
		StateDir:      h.stateDir,
		ConfigDir:     h.configDir,
		CommandRunner: h.runner,
		OpenProcess:   func(int) routerProcess { return h.process },
		Sleep: func(ctx context.Context, _ time.Duration) error {
			h.sleeps++
			if h.sleepHook != nil {
				h.sleepHook(h.sleeps)
			}
			return ctx.Err()
		},
		UntrustCA: func(context.Context, string) error {
			outcome.untrusted = true
			return nil
		},
		RemoveUserPath: func(context.Context, string) error {
			outcome.pathRemoved = true
			return nil
		},
		RemoveFirewallRules: func(context.Context, string) error {
			outcome.firewallRun = true
			return nil
		},
		RemoveState: &removeState,
	})
	outcome.out = out.String()
	outcome.stateRemoved = !exists(h.stateDir)
	return outcome
}

func TestUninstallRefusesDestructiveCleanupWhenStopIsUncertain(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(*stopHarness)
		ctx     func(*stopHarness) context.Context
		want    string
		wantPID string
	}{
		{
			name:    "invalid PID record",
			setup:   func(h *stopHarness) { h.withPID = "garbage\n" },
			want:    "invalid router PID record",
			wantPID: "garbage\n",
		},
		{
			name: "process identity unknown",
			setup: func(h *stopHarness) {
				h.process.before = []processState{processUnknown}
			},
			want:    "unable to verify router process",
			wantPID: "4242\n",
		},
		{
			name: "signal fails while still matched",
			setup: func(h *stopHarness) {
				h.shutdownErr = errors.New("down")
				h.process.terminateErr = errors.New("permission denied")
			},
			want:    "unable to signal router process",
			wantPID: "4242\n",
		},
		{
			name:    "exit timeout",
			setup:   func(h *stopHarness) { h.shutdownErr = errors.New("down") },
			want:    "timed out waiting for router process",
			wantPID: "4242\n",
		},
		{
			name: "systemd stop fails",
			setup: func(h *stopHarness) {
				h.withPID = ""
				h.systemdUnit = true
				h.systemdError = errors.New("unit busy")
			},
			want: "unit busy",
		},
		{
			name: "shutdown accepted without a recorded process",
			setup: func(h *stopHarness) {
				h.withPID = ""
			},
			want: "cannot be confirmed",
		},
		{
			name: "replacement router recorded",
			setup: func(h *stopHarness) {
				h.shutdownErr = errors.New("down")
				h.process.after = []processState{processAbsent}
				h.process.onTerminate = func() {
					writeFile(t, filepath.Join(h.stateDir, router.RouterPIDFilename), "5555\n")
				}
			},
			want:    "runtime record changed",
			wantPID: "5555\n",
		},
		{
			name:  "canceled",
			setup: func(*stopHarness) {},
			ctx: func(h *stopHarness) context.Context {
				ctx, cancel := context.WithCancel(t.Context())
				h.sleepHook = func(int) { cancel() }
				return ctx
			},
			want:    "context canceled",
			wantPID: "4242\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newStopHarness(t)
			tt.setup(h)
			ctx := t.Context()
			if tt.ctx != nil {
				ctx = tt.ctx(h)
			}

			got := h.runUninstall(t, ctx)

			if got.err == nil || !strings.Contains(got.err.Error(), tt.want) {
				t.Fatalf("uninstall error = %v, want %q", got.err, tt.want)
			}
			if got.untrusted || got.pathRemoved || got.firewallRun || got.stateRemoved || got.out != "" {
				t.Fatalf("destructive work ran: %+v", got)
			}
			for _, path := range []string{
				filepath.Join(h.stateDir, "bin", "gohere"),
				filepath.Join(h.stateDir, "bin", "gohere.exe"),
				filepath.Join(h.stateDir, "token"),
				filepath.Join(h.stateDir, "routes.json"),
			} {
				if !exists(path) {
					t.Fatalf("%s was removed", path)
				}
			}
			if h.systemdUnit && !exists(filepath.Join(h.configDir, "systemd", "user", "gohere-router.service")) {
				t.Fatal("systemd unit was removed")
			}
			if h.runner.saw("systemctl", "--user", "disable", "gohere-router") {
				t.Fatal("systemd unit was disabled")
			}
			if record, _ := h.pidRecord(t); record != tt.wantPID {
				t.Fatalf("router.pid = %q, want %q", record, tt.wantPID)
			}
		})
	}
}

func TestUninstallContinuesAfterConcurrentExitDuringSignalError(t *testing.T) {
	h := newStopHarness(t)
	h.shutdownErr = errors.New("down")
	h.systemdUnit = true
	h.process.terminateErr = errors.New("no such process")
	h.process.after = []processState{processAbsent}

	got := h.runUninstall(t, t.Context())

	if got.err != nil {
		t.Fatal(got.err)
	}
	if !got.untrusted || !got.pathRemoved || !got.stateRemoved {
		t.Fatalf("uninstall did not finish: %+v", got)
	}
	if !h.runner.saw("systemctl", "--user", "disable", "gohere-router") {
		t.Fatalf("missing systemctl disable: %#v", h.runner.commands)
	}
	if exists(filepath.Join(h.configDir, "systemd", "user", "gohere-router.service")) {
		t.Fatal("systemd unit still exists")
	}
}

type cancelingRunner struct {
	uninstallRecordingRunner
	cancelOn string
	cancel   context.CancelFunc
}

func (r *cancelingRunner) Run(ctx context.Context, command string, args ...string) error {
	if len(args) > 1 && args[1] == r.cancelOn {
		r.cancel()
	}
	return r.uninstallRecordingRunner.Run(ctx, command, args...)
}

func TestUninstallChecksCancellationBetweenDestructiveStages(t *testing.T) {
	stateDir := t.TempDir()
	configDir := t.TempDir()
	writeFile(t, filepath.Join(stateDir, "bin", "gohere"), "binary")
	writeFile(t, filepath.Join(configDir, "systemd", "user", "gohere-router.service"), "service")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runner := &cancelingRunner{cancelOn: "disable", cancel: cancel}
	removeState := true
	var pathRemoved, untrusted bool

	err := UninstallWithConfig(ctx, io.Discard, UninstallConfig{
		StateDir:       stateDir,
		ConfigDir:      configDir,
		CommandRunner:  runner,
		RemoveUserPath: func(context.Context, string) error { pathRemoved = true; return nil },
		UntrustCA:      func(context.Context, string) error { untrusted = true; return nil },
		RemoveState:    &removeState,
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if pathRemoved || untrusted || !exists(filepath.Join(stateDir, "bin", "gohere")) || !exists(stateDir) {
		t.Fatalf("work continued after cancellation: path %v, untrust %v", pathRemoved, untrusted)
	}
}
