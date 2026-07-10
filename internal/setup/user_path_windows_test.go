//go:build windows

package setup

import (
	"strings"
	"testing"
)

func TestWindowsUserPathScriptQuotesPathAndAvoidsSetx(t *testing.T) {
	script := windowsUserPathScript(`C:\Users\O'Brien\.gohere\bin`)
	if !strings.Contains(script, `O''Brien`) || !strings.Contains(script, "SetEnvironmentVariable('Path'") {
		t.Fatalf("script = %q", script)
	}
	if strings.Contains(strings.ToLower(script), "setx") {
		t.Fatalf("script uses truncating setx: %q", script)
	}
}
