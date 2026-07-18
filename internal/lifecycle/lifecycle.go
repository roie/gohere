package lifecycle

import (
	"context"
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

	"github.com/roie/gohere/internal/bridge"
	"github.com/roie/gohere/internal/probe"
	"github.com/roie/gohere/internal/router"
)

var currentIsWSL = bridge.DetectWSL
var processStartTime = realProcessStartTime
var processIdentity = realProcessIdentity
var stopPID = StopPID
var commandOutput = commandOutputWithTimeout

const processCommandTimeout = 2 * time.Second

type RouteStatusKind string

const (
	RouteStatusStarting RouteStatusKind = "starting"
	RouteStatusReady    RouteStatusKind = "ready"
	RouteStatusDead     RouteStatusKind = "dead"
	RouteStatusUnknown  RouteStatusKind = "unknown"
)

type RouteStatus struct {
	Route  router.Route
	Status RouteStatusKind
}

type StopResult struct {
	Hosts       []string
	MatchedHost string
	Stopped     bool
	Warning     string
	Skipped     []StopSkip
}

type StopSkip struct {
	Host   string
	Reason string
}

func FormatRoutes(statuses []RouteStatus) string {
	if len(statuses) == 0 {
		return "No active routes.\n"
	}

	var out strings.Builder
	fmt.Fprintf(&out, "%-28s %-25s %-8s %s\n", "host", "target", "status", "share")
	for _, status := range statuses {
		fmt.Fprintf(&out, "%-28s %-25s %-8s %s\n", status.Route.Host, status.Route.EffectiveTarget(), status.Status, routeLANShareLabel(status.Route))
	}
	return out.String()
}

func FormatRoutesVerbose(statuses []RouteStatus) string {
	if len(statuses) == 0 {
		return "No active routes.\n"
	}

	var out strings.Builder
	fmt.Fprintf(&out, "%-28s %-25s %-7s %-8s %-8s %-8s %-6s %-20s %-36s %-30s %s\n", "host", "target", "status", "mode", "source", "owner", "pid", "started", "stop", "share", "cwd")
	for _, status := range statuses {
		_, stopReason := RouteStopInfo(status)
		if stopReason == "" {
			stopReason = "ok"
		}
		fmt.Fprintf(&out, "%-28s %-25s %-7s %-8s %-8s %-8s %-6d %-20s %-36s %-30s %s\n",
			status.Route.Host,
			status.Route.EffectiveTarget(),
			status.Status,
			RouteMode(status.Route),
			RouteSource(status.Route),
			RouteOwner(status.Route),
			status.Route.PID,
			RouteStartedAt(status.Route),
			stopReason,
			routeLANShareLabel(status.Route),
			status.Route.CWD)
	}
	return out.String()
}

func routeLANShareLabel(route router.Route) string {
	if route.LANShare == nil {
		return "-"
	}
	hostname := route.LANShare.Hostname
	if hostname == "" {
		hostname = route.LANShare.RequestedHostname
	}
	url := "https://" + strings.TrimSuffix(hostname, ".")
	if route.LANShare.State == router.LANShareActive {
		return url
	}
	return string(route.LANShare.State) + ":" + url
}

func RouteMode(route router.Route) string {
	if route.Mode != "" {
		return route.Mode
	}
	return "unknown"
}

func RouteSource(route router.Route) string {
	if route.Source != "" {
		return route.Source
	}
	return "local"
}

func RouteOwner(route router.Route) string {
	if owner := routeOwnerEnv(route); owner != "" {
		return owner
	}
	return "local"
}

func RouteStartedAt(route router.Route) string {
	if route.StartedAt.IsZero() {
		return "-"
	}
	return route.StartedAt.UTC().Format(time.RFC3339)
}

func RouteStopInfo(status RouteStatus) (bool, string) {
	route := status.Route
	ownerEnv := routeOwnerEnv(route)
	if ownerEnv != "" && ownerEnv != currentOwnerEnv() {
		return false, "route belongs to another environment"
	}
	if status.Status == RouteStatusDead || !PIDAlive(route.PID) {
		return true, ""
	}
	if RouteProcessVerified(route) {
		return true, ""
	}
	return false, UnverifiedProcessWarning(route.PID)
}

func RouteStatuses(routes []router.Route) []RouteStatus {
	return RouteStatusesWithRouterReady(routes, true)
}

func RouteStatusesWithRouterReady(routes []router.Route, routerReady bool) []RouteStatus {
	statuses := make([]RouteStatus, 0, len(routes))
	for _, route := range routes {
		if route.EffectiveState() == router.RouteStatePending {
			statuses = append(statuses, RouteStatus{Route: route, Status: RouteStatusStarting})
			continue
		}
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

	deadRefs := make(map[router.RouteRef]bool)
	deadLegacy := make(map[string]bool)
	now := time.Now()
	for _, route := range routes {
		status := RouteStatusUnknown
		if routerReady {
			status = classifyRoute(route)
		}
		expiredReservation := router.RouteReservationExpired(route, now)
		expiredLease := route.EffectiveState() == router.RouteStateActive && !route.LeaseExpiresAt.IsZero() && router.RouteLeaseExpired(route, now)
		if status != RouteStatusDead && !expiredReservation && !expiredLease {
			continue
		}
		if route.ID != "" && route.Generation != 0 {
			deadRefs[route.Ref()] = true
		} else {
			deadLegacy[routeUpdateKey(route)] = true
		}
	}
	if len(deadRefs) == 0 && len(deadLegacy) == 0 {
		return 0, nil
	}

	removed := 0
	if err := router.UpdateStore(store, func(routes []router.Route) ([]router.Route, error) {
		kept := routes[:0]
		for _, route := range routes {
			matched := deadRefs[route.Ref()]
			if !matched && len(deadLegacy) > 0 {
				matched = deadLegacy[routeUpdateKey(route)]
			}
			if matched {
				removed++
				continue
			}
			kept = append(kept, route)
		}
		return kept, nil
	}); err != nil {
		return 0, err
	}
	return removed, nil
}

func StopCurrent(store router.Store, cwd string) (string, bool, string, error) {
	result, err := StopCWDs(store, []string{cwd})
	return result.MatchedHost, result.Stopped, result.Warning, err
}

func StopCWDs(store router.Store, cwds []string) (StopResult, error) {
	routes, err := store.Load()
	if err != nil {
		return StopResult{}, err
	}
	absCWDs, err := AbsCWDSet(cwds)
	if err != nil {
		return StopResult{}, err
	}

	var result StopResult
	removeRefs := make(map[router.RouteRef]bool)
	removeLegacy := make(map[string]bool)
	for _, route := range routes {
		if RouteMatchesAnyCWD(route, absCWDs) {
			result.MatchedHost = route.Host
			if !PIDAlive(route.PID) || targetStatus(route.Target) == RouteStatusDead {
				result.Hosts = append(result.Hosts, route.Host)
				markRouteRemoval(route, removeRefs, removeLegacy)
				continue
			}
			if RouteProcessVerified(route) {
				stopPID(route.PID)
				result.Hosts = append(result.Hosts, route.Host)
				result.Stopped = true
				markRouteRemoval(route, removeRefs, removeLegacy)
				continue
			}
			if result.Warning == "" {
				result.Warning = UnverifiedProcessWarning(route.PID)
			}
			continue
		}
	}
	if len(removeRefs) > 0 || len(removeLegacy) > 0 {
		if err := router.UpdateStore(store, func(routes []router.Route) ([]router.Route, error) {
			kept := routes[:0]
			for _, route := range routes {
				matched := removeRefs[route.Ref()]
				if !matched && len(removeLegacy) > 0 {
					matched = removeLegacy[routeUpdateKey(route)]
				}
				if matched {
					continue
				}
				kept = append(kept, route)
			}
			return kept, nil
		}); err != nil {
			return result, err
		}
	}
	return result, nil
}

func markRouteRemoval(route router.Route, refs map[router.RouteRef]bool, legacy map[string]bool) {
	if route.ID != "" && route.Generation != 0 {
		refs[route.Ref()] = true
		return
	}
	legacy[routeUpdateKey(route)] = true
}

func routeUpdateKey(route router.Route) string {
	return route.Host + "\x00" +
		route.Target + "\x00" +
		strconv.Itoa(route.PID) + "\x00" +
		route.ProcessIdentity + "\x00" +
		route.StartedAt.UTC().Format(time.RFC3339Nano)
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

func AbsCWDSet(cwds []string) (map[string]bool, error) {
	absCWDs := make(map[string]bool, len(cwds))
	for _, cwd := range cwds {
		if cwd == "" {
			continue
		}
		absCWD, err := filepath.Abs(cwd)
		if err != nil {
			return nil, err
		}
		absCWDs[absCWD] = true
	}
	return absCWDs, nil
}

func RouteMatchesAnyCWD(route router.Route, absCWDs map[string]bool) bool {
	for absCWD := range absCWDs {
		if routeMatchesCWD(route, absCWD) {
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
	if route.EffectiveState() == router.RouteStatePending {
		return RouteStatusStarting
	}
	if route.PID > 0 && routePIDIsLocal(route) && !PIDAlive(route.PID) {
		return RouteStatusDead
	}
	status := targetStatus(route.Target)
	if router.RouteLeaseExpired(route, time.Now()) && status != RouteStatusDead {
		return RouteStatusUnknown
	}
	return status
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
	if runtime.GOOS == "darwin" {
		startedAt, ok := darwinProcessStartTime(pid)
		if !ok {
			return "", false
		}
		return "darwin:" + startedAt.Format(time.RFC3339Nano), true
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
	if runtime.GOOS == "darwin" {
		return darwinProcessStartTime(pid)
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
	output, err := commandOutput(
		processCommandTimeout,
		"powershell.exe",
		"-NoProfile",
		"-Command",
		fmt.Sprintf(`$p = Get-Process -Id %d -ErrorAction Stop; $p.StartTime.ToUniversalTime().ToString("o")`, pid),
	)
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

func darwinProcessStartTime(pid int) (time.Time, bool) {
	output, err := commandOutput(
		processCommandTimeout,
		"ps",
		"-o",
		"lstart=",
		"-p",
		strconv.Itoa(pid),
	)
	if err != nil {
		return time.Time{}, false
	}
	return parseDarwinProcessStartTime(string(output), time.Local)
}

func parseDarwinProcessStartTime(output string, location *time.Location) (time.Time, bool) {
	output = strings.TrimSpace(output)
	if output == "" {
		return time.Time{}, false
	}
	if location == nil {
		location = time.Local
	}
	startedAt, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", output, location)
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
	output, err := commandOutput(processCommandTimeout, "getconf", "CLK_TCK")
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
	output, err := commandOutput(processCommandTimeout, "tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH")
	if err != nil {
		return false
	}
	return tasklistContainsPID(string(output), pid)
}

func commandOutputWithTimeout(timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
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
