package lifecycle

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/roie/gohere/internal/router"
)

type RouteStatus struct {
	Route     router.Route
	Reachable bool
}

func FormatRoutes(statuses []RouteStatus) string {
	if len(statuses) == 0 {
		return "No active routes.\n"
	}

	var out strings.Builder
	for _, status := range statuses {
		reachable := "no"
		if status.Reachable {
			reachable = "yes"
		}
		fmt.Fprintf(&out, "%s -> %s cwd %s pid %d backend %s\n", status.Route.Host, status.Route.Target, status.Route.CWD, status.Route.PID, reachable)
	}
	return out.String()
}

func RouteStatuses(routes []router.Route) []RouteStatus {
	statuses := make([]RouteStatus, 0, len(routes))
	for _, route := range routes {
		statuses = append(statuses, RouteStatus{Route: route, Reachable: targetReachable(route.Target)})
	}
	return statuses
}

func Clean(store router.Store) (int, error) {
	routes, err := store.Load()
	if err != nil {
		return 0, err
	}

	kept := routes[:0]
	removed := 0
	for _, route := range routes {
		if routeAlive(route) {
			kept = append(kept, route)
		} else {
			removed++
		}
	}
	if err := store.Save(kept); err != nil {
		return 0, err
	}
	return removed, nil
}

func routeAlive(route router.Route) bool {
	if route.PID > 0 && !PIDAlive(route.PID) {
		return false
	}
	return targetReachable(route.Target)
}

func StopCurrent(store router.Store, cwd string) (bool, error) {
	routes, err := store.Load()
	if err != nil {
		return false, err
	}
	absCWD, err := filepath.Abs(cwd)
	if err != nil {
		return false, err
	}

	stopped := false
	kept := routes[:0]
	for _, route := range routes {
		routeCWD, err := filepath.Abs(route.CWD)
		if err == nil && routeCWD == absCWD {
			if PIDAlive(route.PID) {
				stopPID(route.PID)
				stopped = true
			}
			continue
		}
		kept = append(kept, route)
	}
	if err := store.Save(kept); err != nil {
		return false, err
	}
	return stopped, nil
}

func targetReachable(target string) bool {
	client := http.Client{Timeout: 200 * time.Millisecond}
	resp, err := client.Get(target)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

func stopPID(pid int) {
	if pid <= 0 {
		return
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	process.Signal(syscall.SIGTERM)
}

func PIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}

func FormatDoctor(checks []DoctorCheck) string {
	var out strings.Builder
	for _, check := range checks {
		status := "ok"
		if !check.OK {
			status = "fail"
		}
		fmt.Fprintf(&out, "%s %s", status, check.Name)
		if check.Detail != "" {
			fmt.Fprintf(&out, " %s", check.Detail)
		}
		out.WriteByte('\n')
	}
	return out.String()
}

type DoctorCheck struct {
	Name   string
	OK     bool
	Detail string
}

func RoutePIDDetail(pid int) string {
	if pid == 0 {
		return ""
	}
	return "pid " + strconv.Itoa(pid)
}
