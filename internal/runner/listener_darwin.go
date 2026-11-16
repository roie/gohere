//go:build darwin

package runner

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func listenerPIDs(ctx context.Context, host string, port int) ([]int, error) {
	output, err := exec.CommandContext(ctx, "/usr/sbin/lsof", "-nP", "-a",
		"-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN", "-Fpn").Output()
	if err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); !ok || exit.ExitCode() != 1 || len(output) != 0 {
			return nil, fmt.Errorf("listener lookup: %w", err)
		}
	}
	return parseListenerPIDs(string(output), host)
}

func parseListenerPIDs(output, host string) ([]int, error) {
	var pids []int
	pid := 0
	for line := range strings.SplitSeq(strings.TrimSpace(output), "\n") {
		if strings.HasPrefix(line, "p") {
			var err error
			pid, err = strconv.Atoi(line[1:])
			if err != nil || pid <= 0 {
				return nil, fmt.Errorf("invalid listener PID %q", line)
			}
		}
		if address, ok := strings.CutPrefix(line, "n"); ok {
			ip, _, err := net.SplitHostPort(address)
			if err != nil || pid <= 0 || (ip != "*" && net.ParseIP(ip) == nil) {
				return nil, fmt.Errorf("invalid listener address %q", line)
			}
			if ip == "*" || net.ParseIP(ip).Equal(net.ParseIP(host)) || net.ParseIP(host).IsUnspecified() {
				pids = append(pids, pid)
			}
		}
	}
	return pids, nil
}

func processParents(ctx context.Context) (map[int]int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	parents := make(map[int]int, len(processes))
	for _, process := range processes {
		parents[int(process.Proc.P_pid)] = int(process.Eproc.Ppid)
	}
	return parents, nil
}
