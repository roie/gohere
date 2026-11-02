package app

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

// windowsRouterProcess keeps one process handle open; the PID cannot be reused
// while the handle exists, so inspection, termination, and exit checks all
// target the same process.
type windowsRouterProcess struct {
	handle  windows.Handle
	absent  bool
	openErr error
}

func openRouterProcess(pid int) routerProcess {
	if pid <= 0 || uint64(pid) > uint64(^uint32(0)) {
		return &windowsRouterProcess{openErr: fmt.Errorf("invalid process ID %d", pid)}
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, uint32(pid))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return &windowsRouterProcess{absent: true}
	}
	if err != nil {
		return &windowsRouterProcess{openErr: err}
	}
	return &windowsRouterProcess{handle: handle}
}

func (p *windowsRouterProcess) exited() (bool, error) {
	event, err := windows.WaitForSingleObject(p.handle, 0)
	switch {
	case err != nil:
		return false, err
	case event == windows.WAIT_OBJECT_0:
		return true, nil
	case event == uint32(windows.WAIT_TIMEOUT):
		return false, nil
	default:
		return false, fmt.Errorf("unexpected process wait result %d", event)
	}
}

func (p *windowsRouterProcess) Inspect(stableBinary string) (processState, error) {
	if p.absent {
		return processAbsent, nil
	}
	if p.openErr != nil {
		return processUnknown, p.openErr
	}
	exited, err := p.exited()
	if err != nil {
		return processUnknown, err
	}
	if exited {
		return processAbsent, nil
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(p.handle, 0, &buf[0], &size); err != nil {
		if exited, waitErr := p.exited(); waitErr == nil && exited {
			return processAbsent, nil
		}
		return processUnknown, err
	}
	if windowsExecutableMatches(windows.UTF16ToString(buf[:size]), stableBinary) {
		return processMatched, nil
	}
	return processMismatched, nil
}

func (p *windowsRouterProcess) Terminate() error {
	if p.handle == 0 {
		return errors.New("router process is not open")
	}
	return windows.TerminateProcess(p.handle, 1)
}

func (p *windowsRouterProcess) Close() error {
	if p.handle == 0 {
		return nil
	}
	err := windows.CloseHandle(p.handle)
	p.handle = 0
	return err
}
