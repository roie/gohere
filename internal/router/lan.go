package router

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type LANShareState string

const (
	LANShareRequested LANShareState = "requested"
	LANShareActive    LANShareState = "active"
	LANShareSuspended LANShareState = "suspended"
	LANShareRemoving  LANShareState = "removing"
)

type LANShare struct {
	State             LANShareState `json:"state"`
	RequestedHostname string        `json:"requestedHostname"`
	Hostname          string        `json:"hostname,omitempty"`
	InterfaceIndex    int           `json:"interfaceIndex,omitempty"`
	InterfaceName     string        `json:"interfaceName,omitempty"`
	Address           string        `json:"address,omitempty"`
	Prefix            string        `json:"prefix,omitempty"`
	CreatedAt         time.Time     `json:"createdAt"`
	LastConnectedAt   time.Time     `json:"lastConnectedAt,omitempty"`
	SuspendedReason   string        `json:"suspendedReason,omitempty"`
}

func RequestLANShare(store Store, ref RouteRef, now time.Time) (Route, error) {
	var result Route
	err := UpdateStore(store, func(routes []Route) ([]Route, error) {
		index := routeRefIndex(routes, ref)
		if index < 0 {
			return nil, ErrRouteRefMismatch
		}
		route := &routes[index]
		if route.EffectiveState() != RouteStateActive {
			return nil, errors.New("LAN sharing requires an active route")
		}
		if route.LANShare == nil {
			share := requestedLANShare("lan", route.Host, now)
			if share == nil {
				return nil, fmt.Errorf("route host %q cannot be shared on LAN", route.Host)
			}
			route.LANShare = share
		}
		result = *route
		return routes, nil
	})
	return result, err
}

func SetLANShareState(store Store, ref RouteRef, next LANShareState) error {
	return UpdateStore(store, func(routes []Route) ([]Route, error) {
		index := routeRefIndex(routes, ref)
		if index < 0 {
			return nil, ErrRouteRefMismatch
		}
		share := routes[index].LANShare
		if share == nil {
			return nil, errors.New("route has no LAN share")
		}
		if share.State == next {
			return routes, nil
		}
		if !validLANShareTransition(share.State, next) {
			return nil, fmt.Errorf("invalid LAN share transition %s to %s", share.State, next)
		}
		share.State = next
		return routes, nil
	})
}

func RemoveLANShare(store Store, ref RouteRef) error {
	return UpdateStore(store, func(routes []Route) ([]Route, error) {
		index := routeRefIndex(routes, ref)
		if index < 0 {
			return nil, ErrRouteRefMismatch
		}
		routes[index].LANShare = nil
		return routes, nil
	})
}

func requestedLANShare(mode, routeHost string, now time.Time) *LANShare {
	if mode != "lan" {
		return nil
	}
	label, ok := strings.CutSuffix(strings.ToLower(routeHost), ".localhost")
	if !ok || label == "" {
		return nil
	}
	return &LANShare{
		State:             LANShareRequested,
		RequestedHostname: label + ".local.",
		CreatedAt:         now.UTC(),
	}
}

func validLANShareTransition(current, next LANShareState) bool {
	switch current {
	case LANShareRequested:
		return next == LANShareActive || next == LANShareSuspended || next == LANShareRemoving
	case LANShareActive:
		return next == LANShareSuspended || next == LANShareRemoving
	case LANShareSuspended:
		return next == LANShareActive || next == LANShareRemoving
	default:
		return false
	}
}
