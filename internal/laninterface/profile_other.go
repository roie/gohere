//go:build !windows

package laninterface

import "context"

func platformNetworkProfile(context.Context, int) (NetworkProfile, error) {
	return ProfileUnknown, nil
}
