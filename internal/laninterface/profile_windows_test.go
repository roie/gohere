//go:build windows

package laninterface

import (
	"strings"
	"testing"
)

func TestWindowsNetworkProfileScriptRequiresExactlyOnePhysicalAdapter(t *testing.T) {
	script := windowsNetworkProfileScript(13)
	for _, want := range []string{
		"Get-NetAdapter -Physical -InterfaceIndex 13",
		"@($adapter).Count -ne 1",
		"Get-NetConnectionProfile -InterfaceIndex 13",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q:\n%s", want, script)
		}
	}
}
