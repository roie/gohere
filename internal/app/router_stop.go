package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"time"

	"github.com/roie/gohere/internal/admin"
	"github.com/roie/gohere/internal/router"
	"github.com/roie/gohere/internal/setup"
)

const (
	routerGracefulStopWait = 4 * time.Second
	routerSignalStopWait   = 5 * time.Second
	routerStopPoll         = 100 * time.Millisecond
	routerHealthProbeWait  = time.Second
)

var errShutdownUnconfirmed = errors.New("router accepted shutdown, but router.pid does not identify a running router, so its exit cannot be confirmed; run the command again")

var adminHealth = func(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, routerHealthProbeWait)
	defer cancel()
	return admin.NewClient("http://127.0.0.1:39399", "").Health(ctx)
}

type routerStopConfig struct {
	stateDir      string
	configDir     string
	commandRunner setup.CommandRunner
	openProcess   func(int) routerProcess
	sleep         func(context.Context, time.Duration) error
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// stopInstalledRouter reports stopped only after the recorded router process
// is observed gone or a synchronous systemd stop succeeds. It signals only a
// matched process, at most once, and never escalates. On any error the PID
// record is left in place for diagnosis and retry.
func stopInstalledRouter(ctx context.Context, cfg routerStopConfig) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	record := router.ReadPIDRecord(cfg.stateDir)
	if record.State == router.PIDRecordUnknown {
		return false, fmt.Errorf("invalid router PID record; inspect or remove it after stopping the router manually: %w", record.Err)
	}

	// Inspect before requesting shutdown: a router that exits quickly must
	// still be recognized as the process whose exit confirms the stop.
	var process routerProcess
	state := processAbsent
	pid := record.PID
	stableBinary := filepath.Join(cfg.stateDir, "bin", stableBinaryName(runtime.GOOS))
	if record.State == router.PIDRecordPresent {
		process = cfg.openProcess(pid)
		defer process.Close()
		var err error
		if state, err = process.Inspect(stableBinary); state == processUnknown {
			return false, fmt.Errorf("unable to verify router process %d: %w", pid, err)
		}
	}

	adminErr := shutdownInstalledService(ctx, cfg.stateDir)
	if err := ctx.Err(); err != nil {
		return false, err
	}
	managerStopped := false
	var managerErr error
	if exists(filepath.Join(cfg.configDir, "systemd", "user", "gohere-router.service")) {
		if err := cfg.commandRunner.Run(ctx, "systemctl", "--user", "stop", "gohere-router"); err != nil {
			managerErr = fmt.Errorf("systemd could not stop gohere-router: %w", err)
		} else {
			managerStopped = true
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
	}

	if state != processMatched {
		if record.State == router.PIDRecordPresent {
			if err := router.RemovePIDRecordIf(cfg.stateDir, pid); err != nil {
				return false, errors.Join(managerErr, err)
			}
		}
		return withoutRouterProcess(ctx, adminErr, managerStopped, managerErr)
	}

	var err error
	confirmed := false
	if adminErr == nil || managerStopped {
		if confirmed, err = waitForRouterExit(ctx, cfg, process, stableBinary, routerGracefulStopWait); err != nil {
			return false, err
		}
	}
	if !confirmed {
		if confirmed, err = terminateRouterProcess(ctx, cfg, process, pid, stableBinary); err != nil {
			return false, err
		}
	}

	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := router.RemovePIDRecordIf(cfg.stateDir, pid); err != nil {
		return false, errors.Join(managerErr, err)
	}
	if managerErr != nil {
		return false, managerErr
	}
	return true, nil
}

func withoutRouterProcess(ctx context.Context, adminErr error, managerStopped bool, managerErr error) (bool, error) {
	switch {
	case managerErr != nil:
		return false, managerErr
	case managerStopped:
		return true, nil
	case adminErr == nil:
		return false, errShutdownUnconfirmed
	}
	if err := adminHealth(ctx); err == nil {
		return false, fmt.Errorf("router is responding but could not be stopped: %w", adminErr)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return false, nil
}

func terminateRouterProcess(ctx context.Context, cfg routerStopConfig, process routerProcess, pid int, stableBinary string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	state, err := process.Inspect(stableBinary)
	switch state {
	case processAbsent, processMismatched:
		return true, nil
	case processUnknown:
		return false, fmt.Errorf("unable to verify router process %d: %w", pid, err)
	}
	if termErr := process.Terminate(); termErr != nil {
		state, err := process.Inspect(stableBinary)
		switch state {
		case processAbsent, processMismatched:
			return true, nil
		case processMatched:
			return false, fmt.Errorf("unable to signal router process %d: %w", pid, termErr)
		default:
			return false, fmt.Errorf("unable to signal router process %d: %w", pid, errors.Join(termErr, err))
		}
	}
	confirmed, err := waitForRouterExit(ctx, cfg, process, stableBinary, routerSignalStopWait)
	if err != nil {
		return false, err
	}
	if !confirmed {
		return false, fmt.Errorf("timed out waiting for router process %d to stop", pid)
	}
	return true, nil
}

func waitForRouterExit(ctx context.Context, cfg routerStopConfig, process routerProcess, stableBinary string, wait time.Duration) (bool, error) {
	for range int(wait / routerStopPoll) {
		if err := cfg.sleep(ctx, routerStopPoll); err != nil {
			return false, err
		}
		state, err := process.Inspect(stableBinary)
		switch state {
		case processAbsent, processMismatched:
			return true, nil
		case processUnknown:
			return false, fmt.Errorf("unable to verify router process: %w", err)
		}
	}
	return false, nil
}
