//go:build !windows

package setup

import "context"

func ensureWindowsUserPath(context.Context, string) error { return nil }
