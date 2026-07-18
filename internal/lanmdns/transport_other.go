//go:build !windows && !linux && !darwin

package lanmdns

import "fmt"

func newPlatformTransport(Interface) (transport, error) {
	return nil, fmt.Errorf("LAN mDNS transport is not implemented on this platform")
}
