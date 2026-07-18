package laninterface

import (
	"errors"
	"net"
	"net/netip"
	"strings"
)

var (
	ErrNoPrivateNetwork = errors.New("no private Wi-Fi or Ethernet network is available")
	ErrPublicNetwork    = errors.New("LAN sharing is disabled on public Windows networks")
	ErrAmbiguous        = errors.New("multiple LAN networks are available and no terminal is available to choose one")
	ErrInvalidChoice    = errors.New("invalid LAN network choice")
)

type NetworkProfile string

const (
	ProfileUnknown NetworkProfile = ""
	ProfilePrivate NetworkProfile = "private"
	ProfilePublic  NetworkProfile = "public"
)

type Candidate struct {
	Index      int
	Name       string
	Prefix     netip.Prefix
	Flags      net.Flags
	HasGateway bool
	Profile    NetworkProfile
}

type Chooser func([]Candidate) (int, error)

func Select(inventory []Candidate, choose Chooser) (Candidate, error) {
	candidates := make([]Candidate, 0, len(inventory))
	sawPublic := false
	for _, candidate := range inventory {
		if !safeCandidate(candidate) {
			continue
		}
		if candidate.Profile == ProfilePublic {
			sawPublic = true
			continue
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		if sawPublic {
			return Candidate{}, ErrPublicNetwork
		}
		return Candidate{}, ErrNoPrivateNetwork
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	if choose == nil {
		return Candidate{}, ErrAmbiguous
	}
	index, err := choose(append([]Candidate(nil), candidates...))
	if err != nil {
		return Candidate{}, err
	}
	if index < 0 || index >= len(candidates) {
		return Candidate{}, ErrInvalidChoice
	}
	return candidates[index], nil
}

func safeCandidate(candidate Candidate) bool {
	if candidate.Index <= 0 || candidate.Name == "" || !candidate.Prefix.IsValid() {
		return false
	}
	if candidate.Flags&net.FlagUp == 0 || candidate.Flags&net.FlagMulticast == 0 {
		return false
	}
	if candidate.Flags&(net.FlagLoopback|net.FlagPointToPoint) != 0 {
		return false
	}
	address := candidate.Prefix.Addr()
	if !address.Is4() || !address.IsPrivate() || address.IsLinkLocalUnicast() {
		return false
	}
	return !unsafeInterfaceName(candidate.Name)
}

func unsafeInterfaceName(name string) bool {
	name = strings.ToLower(name)
	for _, marker := range []string{
		"bridge", "docker", "hamachi", "hyper-v", "podman", "tailscale", "tap", "tunnel",
		"utun", "virtualbox", "vmware", "vpn", "vethernet", "wireguard", "wsl", "zerotier",
	} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return strings.HasPrefix(name, "br-") || strings.HasPrefix(name, "tun") || strings.HasPrefix(name, "wg")
}
