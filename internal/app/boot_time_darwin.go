package app

import (
	"time"

	"golang.org/x/sys/unix"
)

func systemBootTime() (time.Time, error) {
	tv, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(tv.Unix()), nil
}
