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

func TestWindowsUserPathRemovalScriptRemovesOnlyExactDirectory(t *testing.T) {
	script := windowsUserPathRemovalScript(`C:\Users\O'Brien\.gohere\bin`)
	if !strings.Contains(script, `O''Brien`) || !strings.Contains(script, "-ine $dir.TrimEnd") {
		t.Fatalf("script = %q", script)
	}
	if !strings.Contains(script, "SetEnvironmentVariable('Path'") {
		t.Fatalf("script does not persist the filtered PATH: %q", script)
	}
}
