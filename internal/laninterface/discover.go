package laninterface

import (
	"context"
	"fmt"
	"net"
	"net/netip"
)

type interfaceSource interface {
	Interfaces() ([]net.Interface, error)
	Addrs(net.Interface) ([]net.Addr, error)
	Profile(context.Context, int) (NetworkProfile, error)
}

type systemSource struct{}

func (systemSource) Interfaces() ([]net.Interface, error)          { return net.Interfaces() }
func (systemSource) Addrs(iface net.Interface) ([]net.Addr, error) { return iface.Addrs() }
func (systemSource) Profile(ctx context.Context, index int) (NetworkProfile, error) {
	return platformNetworkProfile(ctx, index)
}

func Discover(ctx context.Context) ([]Candidate, error) {
	return discover(ctx, systemSource{})
}

func DiscoverAndSelect(ctx context.Context, choose Chooser) (Candidate, error) {
	candidates, err := Discover(ctx)
	if err != nil {
		return Candidate{}, err
	}
	return Select(candidates, choose)
}

func discover(ctx context.Context, source interfaceSource) ([]Candidate, error) {
	interfaces, err := source.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("enumerate network interfaces: %w", err)
	}
	var candidates []Candidate
	for _, iface := range interfaces {
		addresses, err := source.Addrs(iface)
		if err != nil {
			return nil, fmt.Errorf("enumerate addresses for %s: %w", iface.Name, err)
		}
		var interfaceCandidates []Candidate
		for _, address := range addresses {
			prefix, err := netip.ParsePrefix(address.String())
			if err != nil {
				continue
			}
			candidate := Candidate{Index: iface.Index, Name: iface.Name, Prefix: prefix, Flags: iface.Flags}
			if safeCandidate(candidate) {
				interfaceCandidates = append(interfaceCandidates, candidate)
			}
		}
		if len(interfaceCandidates) == 0 {
			continue
		}
		profile, err := source.Profile(ctx, iface.Index)
		if err != nil {
			return nil, fmt.Errorf("inspect network profile for %s: %w", iface.Name, err)
		}
		for _, candidate := range interfaceCandidates {
			candidate.Profile = profile
			candidates = append(candidates, candidate)
		}
	}
	return candidates, nil
}
