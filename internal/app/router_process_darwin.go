package app

import "golang.org/x/sys/unix"

func processExecutable(pid int) (string, error) {
	buf, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return "", err
	}
	return parseProcArgs2Path(buf)
}
