package app

import (
	"time"

	"golang.org/x/sys/windows"
)

var procGetTickCount64 = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetTickCount64")

func systemBootTime() (time.Time, error) {
	if err := procGetTickCount64.Find(); err != nil {
		return time.Time{}, err
	}
	uptime, _, _ := procGetTickCount64.Call()
	return time.Now().Add(-time.Duration(uptime) * time.Millisecond), nil
}
