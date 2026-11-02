package app

import (
	"os"
	"path/filepath"
	"strconv"
)

func processExecutable(pid int) (string, error) {
	target, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err != nil {
		return "", err
	}
	return linuxExecutableLink(target, os.Lstat), nil
}
