//go:build !windows

package lifecycle

import "time"

func windowsProcessStartTime(int) (time.Time, bool) { return time.Time{}, false }
