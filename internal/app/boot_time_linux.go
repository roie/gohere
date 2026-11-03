package app

import (
	"errors"
	"time"

	"github.com/roie/gohere/internal/lifecycle"
)

func systemBootTime() (time.Time, error) {
	boot, ok := lifecycle.LinuxBootTime()
	if !ok {
		return time.Time{}, errors.New("read boot time from /proc/stat")
	}
	return boot, nil
}
