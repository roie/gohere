package laninterface

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
)

func TestSelectUsesOnlySafePrivatePhysicalInterface(t *testing.T) {
	selected, err := Select([]Candidate{
		candidate("lo", 1, "127.0.0.1/8", net.FlagUp|net.FlagLoopback|net.FlagMulticast),
		candidate("docker0", 2, "172.17.0.1/16", net.FlagUp|net.FlagMulticast),
		candidate("tailscale0", 3, "100.64.0.2/32", net.FlagUp|net.FlagPointToPoint),
		candidate("Wi-Fi", 7, "192.168.1.42/24", net.FlagUp|net.FlagMulticast),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Index != 7 || selected.Prefix.String() != "192.168.1.42/24" {
		t.Fatalf("selected = %#v", selected)
	}
}

func TestSelectDoesNotFollowVPNDefaultRoute(t *testing.T) {
	vpn := candidate("Corporate VPN", 4, "10.8.0.2/24", net.FlagUp|net.FlagMulticast|net.FlagPointToPoint)
	vpn.HasGateway = true
	wifi := candidate("Wi-Fi", 7, "192.168.1.42/24", net.FlagUp|net.FlagMulticast)
	selected, err := Select([]Candidate{vpn, wifi}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Index != wifi.Index {
		t.Fatalf("selected = %#v", selected)
	}
}

func TestSelectRequiresChoiceForSeveralNetworks(t *testing.T) {
	wifi := candidate("Wi-Fi", 7, "192.168.1.42/24", net.FlagUp|net.FlagMulticast)
	ethernet := candidate("Ethernet", 9, "10.0.0.18/24", net.FlagUp|net.FlagMulticast)
	_, err := Select([]Candidate{wifi, ethernet}, nil)
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("Select() error = %v", err)
	}
	selected, err := Select([]Candidate{wifi, ethernet}, func(candidates []Candidate) (int, error) { return 1, nil })
	if err != nil {
		t.Fatal(err)
	}
	if selected.Index != ethernet.Index {
		t.Fatalf("selected = %#v", selected)
	}
}

func TestSelectRejectsPublicWindowsNetwork(t *testing.T) {
	public := candidate("Wi-Fi", 7, "192.168.1.42/24", net.FlagUp|net.FlagMulticast)
	public.Profile = ProfilePublic
	_, err := Select([]Candidate{public}, nil)
	if !errors.Is(err, ErrPublicNetwork) {
		t.Fatalf("Select() error = %v", err)
	}
}

func TestSelectRejectsNoPrivateNetwork(t *testing.T) {
	_, err := Select([]Candidate{
		candidate("Ethernet", 9, "203.0.113.8/24", net.FlagUp|net.FlagMulticast),
	}, nil)
	if !errors.Is(err, ErrNoPrivateNetwork) {
		t.Fatalf("Select() error = %v", err)
	}
}

func TestSelectRejectsInvalidChoice(t *testing.T) {
	wifi := candidate("Wi-Fi", 7, "192.168.1.42/24", net.FlagUp|net.FlagMulticast)
	ethernet := candidate("Ethernet", 9, "10.0.0.18/24", net.FlagUp|net.FlagMulticast)
	_, err := Select([]Candidate{wifi, ethernet}, func([]Candidate) (int, error) { return 4, nil })
	if !errors.Is(err, ErrInvalidChoice) {
		t.Fatalf("Select() error = %v", err)
	}
}

func TestPromptChooserPrintsCandidatesAndUsesSelection(t *testing.T) {
	var output bytes.Buffer
	choose := PromptChooser(strings.NewReader("2\n"), &output)
	selected, err := Select([]Candidate{
		candidate("Wi-Fi", 7, "192.168.1.42/24", net.FlagUp|net.FlagMulticast),
		candidate("Ethernet", 9, "10.0.0.18/24", net.FlagUp|net.FlagMulticast),
	}, choose)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Index != 9 {
		t.Fatalf("selected = %#v", selected)
	}
	for _, want := range []string{"Choose a network for LAN sharing:", "1. Wi-Fi", "192.168.1.42", "2. Ethernet", "10.0.0.18", "Network [1]:"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("prompt missing %q:\n%s", want, output.String())
		}
	}
}

func TestPromptChooserDefaultsToFirstNetwork(t *testing.T) {
	choose := PromptChooser(strings.NewReader("\n"), &bytes.Buffer{})
	index, err := choose([]Candidate{candidate("Wi-Fi", 7, "192.168.1.42/24", net.FlagUp|net.FlagMulticast)})
	if err != nil || index != 0 {
		t.Fatalf("choice = %d, %v", index, err)
	}
}

func TestDiscoverDoesNotInspectProfilesForUnsafeInterfaces(t *testing.T) {
	source := fakeSource{
		interfaces: []net.Interface{
			{Index: 1, Name: "Loopback", Flags: net.FlagUp | net.FlagLoopback | net.FlagMulticast},
			{Index: 7, Name: "Wi-Fi", Flags: net.FlagUp | net.FlagMulticast},
		},
		addresses: map[int][]net.Addr{
			1: {&net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)}},
			7: {&net.IPNet{IP: net.ParseIP("192.168.1.42"), Mask: net.CIDRMask(24, 32)}},
		},
		profiles:      map[int]NetworkProfile{7: ProfilePrivate},
		profileErrors: map[int]error{1: errors.New("loopback has no Windows profile")},
	}
	candidates, err := discover(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].Index != 7 {
		t.Fatalf("candidates = %#v", candidates)
	}
}

func TestDiscoverFiltersAddressesAndCarriesNetworkProfile(t *testing.T) {
	source := fakeSource{
		interfaces: []net.Interface{
			{Index: 2, Name: "docker0", Flags: net.FlagUp | net.FlagMulticast},
			{Index: 7, Name: "Wi-Fi", Flags: net.FlagUp | net.FlagMulticast},
		},
		addresses: map[int][]net.Addr{
			2: {&net.IPNet{IP: net.ParseIP("172.17.0.1"), Mask: net.CIDRMask(16, 32)}},
			7: {
				&net.IPNet{IP: net.ParseIP("192.168.1.42"), Mask: net.CIDRMask(24, 32)},
				&net.IPNet{IP: net.ParseIP("203.0.113.8"), Mask: net.CIDRMask(24, 32)},
			},
		},
		profiles: map[int]NetworkProfile{7: ProfilePrivate},
	}
	candidates, err := discover(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].Index != 7 || candidates[0].Profile != ProfilePrivate {
		t.Fatalf("candidates = %#v", candidates)
	}
}

type fakeSource struct {
	interfaces    []net.Interface
	addresses     map[int][]net.Addr
	profiles      map[int]NetworkProfile
	profileErrors map[int]error
}

func (s fakeSource) Interfaces() ([]net.Interface, error) { return s.interfaces, nil }
func (s fakeSource) Addrs(iface net.Interface) ([]net.Addr, error) {
	return s.addresses[iface.Index], nil
}
func (s fakeSource) Profile(_ context.Context, index int) (NetworkProfile, error) {
	return s.profiles[index], s.profileErrors[index]
}

func candidate(name string, index int, prefix string, flags net.Flags) Candidate {
	return Candidate{Name: name, Index: index, Prefix: netip.MustParsePrefix(prefix), Flags: flags}
}
