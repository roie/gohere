package router

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"
)

type LANShareState string

var errLANConnectionTimestampFresh = errors.New("LAN connection timestamp is fresh")

const (
	LANShareRequested LANShareState = "requested"
	LANShareActive    LANShareState = "active"
	LANShareSuspended LANShareState = "suspended"
	LANShareRemoving  LANShareState = "removing"
)

type LANActivation struct {
	Hostname       string
	InterfaceIndex int
	InterfaceName  string
	Address        string
	Prefix         string
}

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

func ActivateLANShare(store Store, ref RouteRef, activation LANActivation) error {
	hostname, err := normalizeLANHostname(activation.Hostname)
	if err != nil {
		return err
	}
	address, err := netip.ParseAddr(activation.Address)
	if err != nil || !address.Is4() || !address.IsPrivate() {
		return errors.New("LAN activation requires a private IPv4 address")
	}
	prefix, err := netip.ParsePrefix(activation.Prefix)
	if err != nil || !prefix.Addr().Is4() || !prefix.Contains(address) {
		return errors.New("LAN activation prefix does not contain its address")
	}
	if activation.InterfaceIndex <= 0 || strings.TrimSpace(activation.InterfaceName) == "" {
		return errors.New("LAN activation requires a network interface")
	}
	return UpdateStore(store, func(routes []Route) ([]Route, error) {
		index := routeRefIndex(routes, ref)
		if index < 0 {
			return nil, ErrRouteRefMismatch
		}
		share := routes[index].LANShare
		if share == nil {
			return nil, errors.New("route has no LAN share")
		}
		if share.State != LANShareActive && !validLANShareTransition(share.State, LANShareActive) {
			return nil, fmt.Errorf("invalid LAN share transition %s to %s", share.State, LANShareActive)
		}
		share.State = LANShareActive
		share.Hostname = hostname
		share.InterfaceIndex = activation.InterfaceIndex
		share.InterfaceName = activation.InterfaceName
		share.Address = address.String()
		share.Prefix = prefix.String()
		share.SuspendedReason = ""
		return routes, nil
	})
}

func MarkLANShareConnected(store Store, ref RouteRef, now time.Time) error {
	err := UpdateStore(store, func(routes []Route) ([]Route, error) {
		index := routeRefIndex(routes, ref)
		if index < 0 {
			return nil, ErrRouteRefMismatch
		}
		share := routes[index].LANShare
		if share == nil || share.State != LANShareActive {
			return nil, errLANConnectionTimestampFresh
		}
		now = now.UTC()
		if !share.LastConnectedAt.IsZero() && now.Sub(share.LastConnectedAt) < time.Minute {
			return nil, errLANConnectionTimestampFresh
		}
		share.LastConnectedAt = now
		return routes, nil
	})
	if errors.Is(err, errLANConnectionTimestampFresh) {
		return nil
	}
	return err
}

func SuspendLANShare(store Store, ref RouteRef, reason string) error {
	return UpdateStore(store, func(routes []Route) ([]Route, error) {
		index := routeRefIndex(routes, ref)
		if index < 0 {
			return nil, ErrRouteRefMismatch
		}
		share := routes[index].LANShare
		if share == nil {
			return routes, nil
		}
		if share.State != LANShareSuspended && !validLANShareTransition(share.State, LANShareSuspended) {
			return nil, fmt.Errorf("invalid LAN share transition %s to %s", share.State, LANShareSuspended)
		}
		share.State = LANShareSuspended
		share.SuspendedReason = reason
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
