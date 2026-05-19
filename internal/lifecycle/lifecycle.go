package lifecycle

import (
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/roie/gohere/internal/router"
)

type RouteStatusKind string

const (
	RouteStatusReady   RouteStatusKind = "ready"
	RouteStatusDead    RouteStatusKind = "dead"
	RouteStatusUnknown RouteStatusKind = "unknown"
)

type RouteStatus struct {
	Route  router.Route
	Status RouteStatusKind
}

func FormatRoutes(statuses []RouteStatus) string {
	if len(statuses) == 0 {
		return "No active routes.\n"
	}

	var out strings.Builder
	fmt.Fprintf(&out, "%-28s %-25s %s\n", "host", "target", "status")
	for _, status := range statuses {
		fmt.Fprintf(&out, "%-28s %-25s %s\n", status.Route.Host, status.Route.Target, status.Status)
	}
	return out.String()
}

func FormatRoutesVerbose(statuses []RouteStatus) string {
	if len(statuses) == 0 {
		return "No active routes.\n"
	}

	var out strings.Builder
	fmt.Fprintf(&out, "%-28s %-25s %-7s %-6s %s\n", "host", "target", "status", "pid", "cwd")
	for _, status := range statuses {
		fmt.Fprintf(&out, "%-28s %-25s %-7s %-6d %s\n", status.Route.Host, status.Route.Target, status.Status, status.Route.PID, status.Route.CWD)
	}
	return out.String()
}

func RouteStatuses(routes []router.Route) []RouteStatus {
	statuses := make([]RouteStatus, 0, len(routes))
	for _, route := range routes {
		statuses = append(statuses, RouteStatus{Route: route, Status: classifyRoute(route)})
	}
	return statuses
}

func Prune(store router.Store) (int, error) {
	routes, err := store.Load()
	if err != nil {
		return 0, err
	}

	kept := routes[:0]
	removed := 0
	for _, route := range routes {
		if classifyRoute(route) == RouteStatusDead {
			removed++
		} else {
			kept = append(kept, route)
		}
	}
	if err := store.Save(kept); err != nil {
		return 0, err
	}
	return removed, nil
}

func StopCurrent(store router.Store, cwd string) (string, bool, error) {
	routes, err := store.Load()
	if err != nil {
		return "", false, err
	}
	absCWD, err := filepath.Abs(cwd)
	if err != nil {
		return "", false, err
	}

	stopped := false
	stoppedHost := ""
	kept := routes[:0]
	for _, route := range routes {
		routeCWD, err := filepath.Abs(route.CWD)
		if err == nil && routeCWD == absCWD {
			stoppedHost = route.Host
			if PIDAlive(route.PID) {
				stopPID(route.PID)
				stopped = true
			}
			continue
		}
		kept = append(kept, route)
	}
	if err := store.Save(kept); err != nil {
		return stoppedHost, false, err
	}
	return stoppedHost, stopped, nil
}

func classifyRoute(route router.Route) RouteStatusKind {
	if route.PID > 0 && !PIDAlive(route.PID) {
		return RouteStatusDead
	}
	return targetStatus(route.Target)
}

func targetStatus(target string) RouteStatusKind {
	client := http.Client{Timeout: 200 * time.Millisecond}
	resp, err := client.Get(target)
	if err != nil {
		if isDefinitiveConnectionFailure(err) {
			return RouteStatusDead
		}
		return RouteStatusUnknown
	}
	resp.Body.Close()
	return RouteStatusReady
}

func isDefinitiveConnectionFailure(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}

func stopPID(pid int) {
	StopPID(pid)
}

func StopPID(pid int) {
	if pid <= 0 {
		return
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	if runtime.GOOS == "windows" {
		process.Kill()
		return
	}
	process.Signal(syscall.SIGTERM)
}

func PIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if runtime.GOOS == "windows" {
		return windowsPIDAlive(pid)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}

func windowsPIDAlive(pid int) bool {
	output, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH").Output()
	if err != nil {
		return false
	}
	return tasklistContainsPID(string(output), pid)
}

func tasklistContainsPID(output string, pid int) bool {
	reader := csv.NewReader(strings.NewReader(output))
	reader.FieldsPerRecord = -1
	for {
		record, err := reader.Read()
		if err != nil {
			return false
		}
		if len(record) < 2 {
			continue
		}
		got, err := strconv.Atoi(strings.TrimSpace(record[1]))
		if err == nil && got == pid {
			return true
		}
	}
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
		if !check.OK && check.Hint != "" {
			fmt.Fprintf(&out, "  %s\n", check.Hint)
		}
	}
	return out.String()
}

type DoctorCheck struct {
	Name   string
	OK     bool
	Detail string
	Hint   string
}

func RoutePIDDetail(pid int) string {
	if pid == 0 {
		return ""
	}
	return "pid " + strconv.Itoa(pid)
}
