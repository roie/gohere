//go:build !linux

package wsledge

import "errors"

func processAlive(int) bool              { return false }
func stopProcess(int) error              { return errors.New("WSL loopback edge requires Linux") }
func processIdentity(int) (string, bool) { return "", false }
