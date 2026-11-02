//go:build !linux && !darwin && !windows

package app

import (
	"errors"
	"runtime"
)

var errProcessInspectionUnsupported = errors.New("router process inspection is not supported on " + runtime.GOOS)

type unsupportedRouterProcess struct{}

func openRouterProcess(int) routerProcess { return unsupportedRouterProcess{} }

func (unsupportedRouterProcess) Inspect(string) (processState, error) {
	return processUnknown, errProcessInspectionUnsupported
}

func (unsupportedRouterProcess) Terminate() error { return errProcessInspectionUnsupported }

func (unsupportedRouterProcess) Close() error { return nil }
