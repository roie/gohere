package runner

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"
)

var errNoListener = errors.New("no listener owner found")

func listenerOwned(ctx context.Context, host string, port, root int) (bool, error) {
	if root <= 0 || port <= 0 || net.ParseIP(host) == nil {
		return false, errors.New("listener ownership requires a process and an IP endpoint")
	}
	owners, err := listenerPIDs(ctx, host, port)
	if err != nil {
		return false, err
	}
	if len(owners) == 0 {
		return false, errNoListener
	}
	parents, err := processParents(ctx)
	if err != nil {
		return false, err
	}
	return listenerDescendsFrom(owners, parents, root)
}

func listenerDescendsFrom(owners []int, parents map[int]int, root int) (bool, error) {
	for _, pid := range owners {
		seen := make(map[int]bool)
		for pid != root {
			if pid == 0 || pid == os.Getpid() {
				return false, nil
			}
			parent, ok := parents[pid]
			if !ok || seen[pid] {
				return false, fmt.Errorf("ancestry unavailable for PID %d", pid)
			}
			seen[pid] = true
			pid = parent
		}
	}
	return true, nil
}

func (r *Result) confirmListener(port int) error {
	host := r.config.ListenHost
	if host == "" || host == "localhost" {
		host = "127.0.0.1"
	}
	ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
	defer cancel()
	endpoint := net.JoinHostPort(host, strconv.Itoa(port))
	for {
		select {
		case <-r.done:
			return r.startupError()
		default:
		}
		owned, err := listenerOwned(ctx, host, port, r.PID())
		if err == nil && owned {
			select {
			case <-r.done:
				return r.startupError()
			default:
			}
			r.Port = port
			return nil
		}
		if err == nil {
			return fmt.Errorf("listener at %s belongs to another process; refusing to publish route", endpoint)
		}
		if !errors.Is(err, errNoListener) {
			return fmt.Errorf("could not establish listener ownership at %s; refusing to publish route: %w", endpoint, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("could not establish listener ownership at %s; refusing to publish route: %w", endpoint, ctx.Err())
		case <-r.done:
			return r.startupError()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
