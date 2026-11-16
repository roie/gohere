//go:build darwin

package runner

import (
	"slices"
	"testing"
)

func TestParseListenerPIDs(t *testing.T) {
	for _, test := range []struct {
		name    string
		output  string
		host    string
		owners  []int
		unknown bool
	}{
		{"exact", "p42\nn127.0.0.1:3000\n", "127.0.0.1", []int{42}, false},
		{"wildcard", "p42\nn*:3000\n", "127.0.0.1", []int{42}, false},
		{"ipv6", "p42\nn[::1]:3000\n", "::1", []int{42}, false},
		{"other-address", "p42\nn127.0.0.2:3000\n", "127.0.0.1", nil, false},
		{"wsl-wildcard", "p42\nn172.20.10.2:3000\n", "0.0.0.0", []int{42}, false},
		{"multiple", "p42\nn127.0.0.1:3000\np43\nn*:3000\n", "127.0.0.1", []int{42, 43}, false},
		{"missing-pid", "n127.0.0.1:3000\n", "127.0.0.1", nil, true},
		{"invalid-pid", "punknown\nn*:3000\n", "127.0.0.1", nil, true},
		{"invalid-address", "p42\nnnot-an-ip:3000\n", "127.0.0.1", nil, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			owners, err := parseListenerPIDs(test.output, test.host)
			if !slices.Equal(owners, test.owners) || (err != nil) != test.unknown {
				t.Fatalf("owners=%v error=%v", owners, err)
			}
		})
	}
}
