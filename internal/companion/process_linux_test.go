//go:build linux

package companion

import (
	"os/exec"
	"testing"
)

func TestConfigureProcessIsolatesCompanionFromTerminalSignals(t *testing.T) {
	command := exec.Command("true")
	configureProcess(command)
	if command.SysProcAttr == nil || !command.SysProcAttr.Setpgid {
		t.Fatalf("SysProcAttr = %#v, want isolated process group", command.SysProcAttr)
	}
}
