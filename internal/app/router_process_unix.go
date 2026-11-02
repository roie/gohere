//go:build linux || darwin

package app

import (
	"errors"
	"os"
	"syscall"
)

type unixRouterProcess struct {
	pid     int
	process *os.Process
}

// openRouterProcess uses os.FindProcess, which holds a pidfd on Linux 5.4+, so
// signals sent through it cannot reach a process that later reuses the PID.
func openRouterProcess(pid int) routerProcess {
	process, _ := os.FindProcess(pid)
	return &unixRouterProcess{pid: pid, process: process}
}

func (p *unixRouterProcess) exists() (bool, error) {
	err := p.process.Signal(syscall.Signal(0))
	switch {
	case err == nil, errors.Is(err, syscall.EPERM):
		return true, nil
	case errors.Is(err, os.ErrProcessDone), errors.Is(err, syscall.ESRCH):
		return false, nil
	default:
		return false, err
	}
}

func (p *unixRouterProcess) Inspect(stableBinary string) (processState, error) {
	alive, err := p.exists()
	if err != nil {
		return processUnknown, err
	}
	if !alive {
		return processAbsent, nil
	}
	state, inspectErr := processUnknown, error(nil)
	if exe, err := processExecutable(p.pid); err != nil {
		inspectErr = err
	} else {
		state, inspectErr = compareUnixExecutable(exe, stableBinary)
	}
	// The path lookup is by PID; probe again so a concurrent exit is reported
	// as absent rather than as the identity of a process that reused the PID.
	alive, err = p.exists()
	if err != nil {
		return processUnknown, errors.Join(inspectErr, err)
	}
	if !alive {
		return processAbsent, nil
	}
	return state, inspectErr
}

func (p *unixRouterProcess) Terminate() error {
	return p.process.Signal(syscall.SIGTERM)
}

func (p *unixRouterProcess) Close() error {
	return p.process.Release()
}
