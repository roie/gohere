//go:build !linux

package wsledge

import "errors"

func processAlive(int) bool                 { return false }
func stopProcess(int) error                 { return errors.New("WSL loopback edge requires Linux") }
func processArguments(int) ([]string, bool) { return nil, false }
func processExecutable(int) (string, bool)  { return "", false }
func processIdentity(int) (string, bool)    { return "", false }
