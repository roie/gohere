//go:build !linux

package companion

import "os/exec"

func configureProcess(*exec.Cmd) {}
