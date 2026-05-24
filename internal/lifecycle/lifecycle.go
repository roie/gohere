package lifecycle

import (
	"encoding/csv"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/roie/gohere/internal/probe"
	"github.com/roie/gohere/internal/router"
)

var currentIsWSL = detectCurrentWSL
var processStartTime = realProcessStartTime
var processIdentity = realProcessIdentity
var stopPID = StopPID

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
	return RouteStatusesWithRouterReady(routes, true)
}

func RouteStatusesWithRouterReady(routes []router.Route, routerReady bool) []RouteStatus {
	statuses := make([]RouteStatus, 0, len(routes))
	for _, route := range routes {
		if !routerReady {
			statuses = append(statuses, RouteStatus{Route: route, Status: RouteStatusUnknown})
			continue
		}
		statuses = append(statuses, RouteStatus{Route: route, Status: classifyRoute(route)})
	}
	return statuses
}

func Prune(store router.Store) (int, error) {
	return PruneWithRouterReady(store, true)
}

func PruneWithRouterReady(store router.Store, routerReady bool) (int, error) {
	routes, err := store.Load()
	if err != nil {
		return 0, err
	}

	kept := routes[:0]
	removed := 0
	for _, route := range routes {
		status := RouteStatusUnknown
		if routerReady {
			status = classifyRoute(route)
		}
		if status == RouteStatusDead {
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

func StopCurrent(store router.Store, cwd string) (string, bool, string, error) {
	routes, err := store.Load()
	if err != nil {
		return "", false, "", err
	}
	absCWD, err := filepath.Abs(cwd)
	if err != nil {
		return "", false, "", err
	}

	stopped := false
	stoppedHost := ""
	warning := ""
	kept := routes[:0]
	for _, route := range routes {
		if routeMatchesCWD(route, absCWD) {
			stoppedHost = route.Host
			if !PIDAlive(route.PID) || targetStatus(route.Target) == RouteStatusDead {
				continue
			}
			if RouteProcessVerified(route) {
				stopPID(route.PID)
				stopped = true
				continue
			}
			warning = UnverifiedProcessWarning(route.PID)
			kept = append(kept, route)
			continue
		}
		kept = append(kept, route)
	}
	if err := store.Save(kept); err != nil {
		return stoppedHost, false, warning, err
	}
	return stoppedHost, stopped, warning, nil
}

func routeMatchesCWD(route router.Route, absCWD string) bool {
	for _, cwd := range []string{route.OwnerCWD, route.CWD} {
		if cwd == "" {
			continue
		}
		routeCWD, err := filepath.Abs(cwd)
		if err == nil && routeCWD == absCWD {
			return true
		}
	}
	return false
}

func UnverifiedProcessWarning(pid int) string {
	if pid <= 0 {
		return "Could not verify the original gohere process. Not stopping PID."
	}
	return fmt.Sprintf("Could not verify the original gohere process. Not stopping PID %d.", pid)
}

func classifyRoute(route router.Route) RouteStatusKind {
	if route.PID > 0 && routePIDIsLocal(route) && !PIDAlive(route.PID) {
		return RouteStatusDead
	}
	return targetStatus(route.Target)
}

func routePIDIsLocal(route router.Route) bool {
	ownerEnv := routeOwnerEnv(route)
	if ownerEnv == "" {
		return true
	}
	return ownerEnv == currentOwnerEnv()
}

func routeOwnerEnv(route router.Route) string {
	if route.OwnerEnv != "" {
		return route.OwnerEnv
	}
	if route.Source == "wsl" {
		return "wsl"
	}
	return ""
}

func currentOwnerEnv() string {
	if currentIsWSL() {
		return "wsl"
	}
	return runtime.GOOS
}

func detectCurrentWSL() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if os.Getenv("WSL_DISTRO_NAME") != "" || os.Getenv("WSL_INTEROP") != "" {
		return true
	}
	data, err := os.ReadFile("/proc/version")
	if err != nil {
		return false
	}
	version := strings.ToLower(string(data))
	return strings.Contains(version, "microsoft") || strings.Contains(version, "wsl")
}

func targetStatus(target string) RouteStatusKind {
	return RouteStatusKind(probe.TargetStatus(target))
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
		taskkill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
		if err := taskkill.Run(); err == nil {
			return
		}
		process.Kill()
		return
	}
	if stopProcessGroup(pid) {
		return
	}
	if err := process.Signal(syscall.SIGTERM); err == nil {
		time.Sleep(500 * time.Millisecond)
		_ = process.Kill()
	}
}

func RouteProcessVerified(route router.Route) bool {
	if route.PID <= 0 {
		return false
	}
	if route.ProcessIdentity != "" {
		identity, ok := processIdentity(route.PID)
		return ok && identity == route.ProcessIdentity
	}
	if route.StartedAt.IsZero() {
		return false
	}
	startedAt, ok := processStartTime(route.PID)
	if !ok {
		return false
	}
	return !startedAt.After(route.StartedAt)
}

func ProcessIdentity(pid int) (string, bool) {
	return processIdentity(pid)
}

func realProcessIdentity(pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	if runtime.GOOS == "linux" {
		ticks, ok := linuxProcessStartTicks(pid)
		if !ok {
			return "", false
		}
		return fmt.Sprintf("linux:%d", ticks), true
	}
	if runtime.GOOS == "windows" {
		startedAt, ok := windowsProcessStartTime(pid)
		if !ok {
			return "", false
		}
		return "windows:" + startedAt.Format(time.RFC3339Nano), true
	}
	return "", false
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

func realProcessStartTime(pid int) (time.Time, bool) {
	if pid <= 0 {
		return time.Time{}, false
	}
	if runtime.GOOS == "windows" {
		return windowsProcessStartTime(pid)
	}
	if runtime.GOOS != "linux" {
		return time.Time{}, false
	}
	ticks, ok := linuxProcessStartTicks(pid)
	if !ok {
		return time.Time{}, false
	}
	bootTime, ok := linuxBootTime()
	if !ok {
		return time.Time{}, false
	}
	hz := linuxClockTicks()
	if hz <= 0 {
		return time.Time{}, false
	}
	return bootTime.Add(time.Duration(ticks) * time.Second / time.Duration(hz)), true
}

func windowsProcessStartTime(pid int) (time.Time, bool) {
	output, err := exec.Command(
		"powershell.exe",
		"-NoProfile",
		"-Command",
		fmt.Sprintf(`$p = Get-Process -Id %d -ErrorAction Stop; $p.StartTime.ToUniversalTime().ToString("o")`, pid),
	).Output()
	if err != nil {
		return time.Time{}, false
	}
	return parseWindowsProcessStartTime(string(output))
}

func parseWindowsProcessStartTime(output string) (time.Time, bool) {
	output = strings.TrimSpace(output)
	if output == "" {
		return time.Time{}, false
	}
	startedAt, err := time.Parse(time.RFC3339Nano, output)
	if err != nil {
		return time.Time{}, false
	}
	return startedAt, true
}

func linuxProcessStartTicks(pid int) (uint64, bool) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, false
	}
	stat := string(data)
	idx := strings.LastIndex(stat, ") ")
	if idx == -1 {
		return 0, false
	}
	fields := strings.Fields(stat[idx+2:])
	if len(fields) <= 19 {
		return 0, false
	}
	ticks, err := strconv.ParseUint(fields[19], 10, 64)
	return ticks, err == nil
}

func linuxBootTime() (time.Time, bool) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "btime ") {
			continue
		}
		seconds, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "btime ")), 10, 64)
		if err != nil {
			return time.Time{}, false
		}
		return time.Unix(seconds, 0), true
	}
	return time.Time{}, false
}

func linuxClockTicks() int64 {
	output, err := exec.Command("getconf", "CLK_TCK").Output()
	if err != nil {
		return 100
	}
	ticks, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
	if err != nil || ticks <= 0 {
		return 100
	}
	return ticks
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
		for _, field := range record {
			got, err := strconv.Atoi(strings.TrimSpace(field))
			if err == nil && got == pid {
				return true
			}
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
