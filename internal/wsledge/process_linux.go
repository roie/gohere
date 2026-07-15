//go:build linux

package wsledge

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, os.ErrPermission)
}

func stopProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Signal(syscall.SIGTERM)
}

func processArguments(pid int) ([]string, bool) {
	if pid <= 0 {
		return nil, false
	}
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil || len(data) == 0 {
		return nil, false
	}
	parts := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
	return parts, len(parts) > 0
}

func processExecutable(pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	path, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err != nil {
		return "", false
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	return path, true
}

func processIdentity(pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", false
	}
	stat := string(data)
	index := strings.LastIndex(stat, ") ")
	if index == -1 {
		return "", false
	}
	fields := strings.Fields(stat[index+2:])
	if len(fields) <= 19 {
		return "", false
	}
	ticks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return "", false
	}
	return fmt.Sprintf("linux:%d", ticks), true
}
