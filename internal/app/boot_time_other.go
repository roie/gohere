//go:build !linux && !darwin && !windows

package app

import "time"

func systemBootTime() (time.Time, error) {
	return time.Time{}, errProcessInspectionUnsupported
}
