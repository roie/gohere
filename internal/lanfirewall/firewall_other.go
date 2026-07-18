//go:build !windows

package lanfirewall

import "context"

func Ensure(context.Context) error { return nil }
