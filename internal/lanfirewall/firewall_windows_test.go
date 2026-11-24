//go:build windows

package lanfirewall

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestWindowsFirewallRemovalScriptSucceedsWithoutMatchingRules(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "gohere.exe")
	output, err := exec.CommandContext(t.Context(), "powershell.exe", "-NoProfile", "-NonInteractive", "-EncodedCommand", encodedPowerShell(windowsFirewallRemovalScript(executable))).CombinedOutput()
	if err != nil {
		t.Fatalf("removal without matching rules failed: %v\n%s", err, output)
	}
}
