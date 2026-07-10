//go:build !linux

package wsledge

import (
	"context"
	"errors"
)

func StartDetached(context.Context, string, string, string) (int, error) {
	return 0, errors.New("WSL loopback edge requires Linux")
}
