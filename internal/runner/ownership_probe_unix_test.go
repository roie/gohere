//go:build !windows

package runner

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func probeListenerPIDs(host string, port int) ([]int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "lsof", "-nP", "-t", "-a",
		"-iTCP@"+net.JoinHostPort(host, strconv.Itoa(port)), "-sTCP:LISTEN").Output()
	if err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); !ok || exit.ExitCode() != 1 || len(output) != 0 {
			return nil, fmt.Errorf("listener lookup: %w", err)
		}
	}
	var pids []int
	for field := range strings.FieldsSeq(string(output)) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			return nil, err
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

func probeProcessParents() (map[int]int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=", "-o", "ppid=").Output()
	if err != nil {
		return nil, err
	}
	parents := make(map[int]int)
	for line := range strings.SplitSeq(strings.TrimSpace(string(output)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("invalid process row: %q", line)
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			return nil, err
		}
		parent, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, err
		}
		parents[pid] = parent
	}
	return parents, nil
}
