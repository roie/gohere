package router

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/roie/gohere/internal/project"
)

var (
	ErrReservationConflict = errors.New("route reservation conflict")
	ErrRouteRefMismatch    = errors.New("route identity does not match")
)

type ReservationRequest struct {
	RunID  string             `json:"runId"`
	TTL    time.Duration      `json:"-"`
	Routes []RouteReservation `json:"routes"`
}

type RouteReservation struct {
	DesiredHost     string    `json:"desiredHost"`
	Service         string    `json:"service,omitempty"`
	PreferredScheme string    `json:"preferredScheme,omitempty"`
	Target          string    `json:"target"`
	CWD             string    `json:"cwd"`
	ProjectRoot     string    `json:"projectRoot,omitempty"`
	ProjectName     string    `json:"projectName,omitempty"`
	Source          string    `json:"source,omitempty"`
	OwnerCWD        string    `json:"ownerCwd,omitempty"`
	OwnerEnv        string    `json:"ownerEnv,omitempty"`
	OwnerInstance   string    `json:"ownerInstance,omitempty"`
	Distro          string    `json:"distro,omitempty"`
	LinuxUser       string    `json:"linuxUser,omitempty"`
	ListenTarget    string    `json:"listenTarget,omitempty"`
	PID             int       `json:"pid,omitempty"`
	Mode            string    `json:"mode,omitempty"`
	ProcessIdentity string    `json:"processIdentity,omitempty"`
	RunnerID        string    `json:"runnerId,omitempty"`
	Reuse           *RouteRef `json:"reuse,omitempty"`
}

type ReservationResult struct {
	RunID  string          `json:"runId"`
	Routes []ReservedRoute `json:"routes"`
}

type ReservedRoute struct {
	Route  Route `json:"route"`
	Reused bool  `json:"reused"`
}

func (r ReservationResult) PendingRefs() []RouteRef {
	refs := make([]RouteRef, 0, len(r.Routes))
	for _, reserved := range r.Routes {
		if !reserved.Reused && reserved.Route.EffectiveState() == RouteStatePending {
			refs = append(refs, reserved.Route.Ref())
		}
	}
	return refs
}

func (r Route) Ref() RouteRef {
	return RouteRef{ID: r.ID, Generation: r.Generation}
}

func ReserveRoutes(store Store, request ReservationRequest, now time.Time) (ReservationResult, error) {
	if store == nil {
		return ReservationResult{}, errors.New("route store is required")
	}
	if strings.TrimSpace(request.RunID) == "" {
		return ReservationResult{}, errors.New("run ID is required")
	}
	if request.TTL <= 0 {
		return ReservationResult{}, errors.New("reservation TTL must be positive")
	}
	if len(request.Routes) == 0 {
		return ReservationResult{}, errors.New("at least one route is required")
	}

	result := ReservationResult{RunID: request.RunID}
	err := UpdateStore(store, func(stored []Route) ([]Route, error) {
		routes := removeExpiredReservations(stored, now)
		occupiedHosts := make(map[string]string, len(routes)+len(request.Routes))
		occupiedTargets := make(map[string]bool, len(routes)+len(request.Routes))
		for _, route := range routes {
			occupiedHosts[route.Host] = "\x00occupied"
			target := route.Target
			if route.EffectiveState() == RouteStatePending {
				target = route.PendingTarget
			}
			if key, err := reservationTargetKey(target); err == nil && key != "" {
				occupiedTargets[key] = true
			}
		}

		reserved := make([]ReservedRoute, 0, len(request.Routes))
		for _, candidate := range request.Routes {
			normalized, err := validateReservation(candidate)
			if err != nil {
				return nil, err
			}
			candidate = normalized
			if candidate.Reuse != nil {
				existing, ok := routeByRef(routes, *candidate.Reuse)
				if !ok || existing.EffectiveState() != RouteStateActive || !reservationOwnerMatches(existing, candidate) {
					return nil, ErrRouteRefMismatch
				}
				reserved = append(reserved, ReservedRoute{Route: existing, Reused: true})
				continue
			}

			targetKey, err := reservationTargetKey(candidate.Target)
			if err != nil {
				return nil, err
			}
			if occupiedTargets[targetKey] {
				return nil, fmt.Errorf("%w: target %s is already reserved", ErrReservationConflict, candidate.Target)
			}
			finalHost := project.ResolveHostnameConflict(candidate.DesiredHost, candidate.CWD, occupiedHosts)
			id, err := newRouteID()
			if err != nil {
				return nil, err
			}
			ownerCWD := candidate.OwnerCWD
			if ownerCWD == "" {
				ownerCWD = candidate.CWD
			}
			route := Route{
				ID:                   id,
				Generation:           1,
				RunID:                request.RunID,
				State:                RouteStatePending,
				Service:              candidate.Service,
				PreferredScheme:      candidate.PreferredScheme,
				PendingTarget:        candidate.Target,
				ReservationExpiresAt: now.Add(request.TTL),
				Host:                 finalHost,
				CWD:                  candidate.CWD,
				Name:                 strings.TrimSuffix(finalHost, ".localhost"),
				ProjectRoot:          candidate.ProjectRoot,
				ProjectName:          candidate.ProjectName,
				Source:               candidate.Source,
				OwnerCWD:             ownerCWD,
				OwnerEnv:             candidate.OwnerEnv,
				OwnerInstance:        candidate.OwnerInstance,
				Distro:               candidate.Distro,
				LinuxUser:            candidate.LinuxUser,
				RunnerID:             candidate.RunnerID,
				ListenTarget:         candidate.ListenTarget,
				PID:                  candidate.PID,
				Mode:                 candidate.Mode,
				ProcessIdentity:      candidate.ProcessIdentity,
				StartedAt:            now,
			}
			if route.RunnerID == "" {
				route.RunnerID = request.RunID
			}
			routes = append(routes, route)
			occupiedHosts[finalHost] = "\x00occupied"
			occupiedTargets[targetKey] = true
			reserved = append(reserved, ReservedRoute{Route: route})
		}
		result.Routes = reserved
		sortRoutes(routes)
		return routes, nil
	})
	if err != nil {
		return ReservationResult{}, err
	}
	return result, nil
}

func ActivateRoutes(store Store, runID string, refs []RouteRef, now time.Time, leaseTTL time.Duration) ([]Route, error) {
	if leaseTTL <= 0 {
		return nil, errors.New("lease TTL must be positive")
	}
	var activated []Route
	err := UpdateStore(store, func(routes []Route) ([]Route, error) {
		indexes, err := completeRunIndexes(routes, runID, refs, RouteStatePending, now)
		if err != nil {
			return nil, err
		}
		activated = make([]Route, len(indexes))
		for i, index := range indexes {
			route := routes[index]
			route.State = RouteStateActive
			route.Target = route.PendingTarget
			route.PendingTarget = ""
			route.ReservationExpiresAt = time.Time{}
			route.LeaseExpiresAt = now.Add(leaseTTL)
			routes[index] = route
			activated[i] = route
		}
		sortRoutes(routes)
		return routes, nil
	})
	return activated, err
}

func ReleaseRoutes(store Store, runID string, refs []RouteRef) error {
	return UpdateStore(store, func(routes []Route) ([]Route, error) {
		indexes, err := completeRunIndexes(routes, runID, refs, "", time.Time{})
		if err != nil {
			return nil, err
		}
		remove := make(map[int]bool, len(indexes))
		for _, index := range indexes {
			remove[index] = true
		}
		next := make([]Route, 0, len(routes)-len(remove))
		for i, route := range routes {
			if !remove[i] {
				next = append(next, route)
			}
		}
		return next, nil
	})
}

func RenewRoutes(store Store, runID string, refs []RouteRef, now time.Time, ttl time.Duration) error {
	if ttl <= 0 {
		return errors.New("route TTL must be positive")
	}
	return UpdateStore(store, func(routes []Route) ([]Route, error) {
		if strings.TrimSpace(runID) == "" || len(refs) == 0 {
			return nil, ErrRouteRefMismatch
		}
		indexes := make([]int, 0, len(refs))
		seen := make(map[RouteRef]bool, len(refs))
		for _, ref := range refs {
			if seen[ref] {
				return nil, ErrRouteRefMismatch
			}
			seen[ref] = true
			index := routeRefIndex(routes, ref)
			if index < 0 || routes[index].RunID != runID {
				return nil, ErrRouteRefMismatch
			}
			if routes[index].EffectiveState() == RouteStatePending && RouteReservationExpired(routes[index], now) {
				return nil, ErrRouteRefMismatch
			}
			indexes = append(indexes, index)
		}
		for _, index := range indexes {
			if routes[index].EffectiveState() == RouteStatePending {
				routes[index].ReservationExpiresAt = now.Add(ttl)
			} else {
				routes[index].LeaseExpiresAt = now.Add(ttl)
			}
		}
		return routes, nil
	})
}

func DeleteRouteRef(store Store, ref RouteRef) error {
	return UpdateStore(store, func(routes []Route) ([]Route, error) {
		index := routeRefIndex(routes, ref)
		if index < 0 {
			return nil, ErrRouteRefMismatch
		}
		return append(routes[:index], routes[index+1:]...), nil
	})
}

func RouteReservationExpired(route Route, now time.Time) bool {
	return route.EffectiveState() == RouteStatePending && !route.ReservationExpiresAt.IsZero() && !route.ReservationExpiresAt.After(now)
}

func removeExpiredReservations(routes []Route, now time.Time) []Route {
	result := make([]Route, 0, len(routes))
	for _, route := range routes {
		if RouteReservationExpired(route, now) {
			continue
		}
		result = append(result, route)
	}
	return result
}

func validateReservation(candidate RouteReservation) (RouteReservation, error) {
	host, err := normalizeRouteHost(candidate.DesiredHost)
	if err != nil {
		return RouteReservation{}, err
	}
	candidate.DesiredHost = host
	if strings.TrimSpace(candidate.CWD) == "" {
		return RouteReservation{}, errors.New("route CWD is required")
	}
	if _, err := reservationTargetKey(candidate.Target); err != nil {
		return RouteReservation{}, err
	}
	if candidate.PreferredScheme != "" && candidate.PreferredScheme != "http" && candidate.PreferredScheme != "https" {
		return RouteReservation{}, errors.New("preferred scheme must be http or https")
	}
	return candidate, nil
}

func reservationTargetKey(target string) (string, error) {
	parsed, err := url.Parse(target)
	if err != nil {
		return "", err
	}
	if err := validateRouteTarget(parsed); err != nil {
		return "", err
	}
	return strings.ToLower(parsed.Host), nil
}

func reservationOwnerMatches(route Route, candidate RouteReservation) bool {
	if !sameOwnerCWD(route, Route{CWD: candidate.CWD, OwnerCWD: candidate.OwnerCWD}) {
		return false
	}
	if candidate.ProjectRoot != "" && route.ProjectRoot != candidate.ProjectRoot {
		return false
	}
	if candidate.ProjectName != "" && route.ProjectName != candidate.ProjectName {
		return false
	}
	return true
}

func routeByRef(routes []Route, ref RouteRef) (Route, bool) {
	index := routeRefIndex(routes, ref)
	if index < 0 {
		return Route{}, false
	}
	return routes[index], true
}

func routeRefIndex(routes []Route, ref RouteRef) int {
	if ref.ID == "" || ref.Generation == 0 {
		return -1
	}
	for i := range routes {
		if routes[i].ID == ref.ID && routes[i].Generation == ref.Generation {
			return i
		}
	}
	return -1
}

func completeRunIndexes(routes []Route, runID string, refs []RouteRef, requiredState RouteState, now time.Time) ([]int, error) {
	indexes, err := matchingRunIndexes(routes, runID, refs, requiredState, now)
	if err != nil {
		return nil, err
	}
	expected := 0
	for _, route := range routes {
		if route.RunID == runID && (requiredState == "" || route.EffectiveState() == requiredState) {
			expected++
		}
	}
	if expected != len(indexes) {
		return nil, ErrRouteRefMismatch
	}
	return indexes, nil
}

func matchingRunIndexes(routes []Route, runID string, refs []RouteRef, requiredState RouteState, now time.Time) ([]int, error) {
	if runID == "" || len(refs) == 0 {
		return nil, ErrRouteRefMismatch
	}
	indexes := make([]int, len(refs))
	seen := make(map[int]bool, len(refs))
	for i, ref := range refs {
		index := routeRefIndex(routes, ref)
		if index < 0 || seen[index] || routes[index].RunID != runID {
			return nil, ErrRouteRefMismatch
		}
		state := routes[index].EffectiveState()
		if requiredState != "" && state != requiredState {
			return nil, ErrRouteRefMismatch
		}
		if state == RouteStatePending && !now.IsZero() && !routes[index].ReservationExpiresAt.IsZero() && !routes[index].ReservationExpiresAt.After(now) {
			return nil, ErrRouteRefMismatch
		}
		seen[index] = true
		indexes[i] = index
	}
	return indexes, nil
}

func newRouteID() (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", fmt.Errorf("generate route ID: %w", err)
	}
	return hex.EncodeToString(data), nil
}
