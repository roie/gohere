package lanfirewall

import (
	"strings"
	"testing"
)

func TestWindowsFirewallScriptScopesRulesToExecutablePrivateProfileAndLocalSubnet(t *testing.T) {
	script := windowsFirewallScript(`C:\Program Files\gohere\gohere.exe`)
	for _, want := range []string{
		"gohere LAN HTTPS", "gohere LAN onboarding", "gohere LAN mDNS",
		"Profile Private", "RemoteAddress LocalSubnet", "Direction Inbound", "Action Allow",
		"Protocol = 'TCP'", "Port = '443'", "Port = '80'", "Protocol = 'UDP'", "Port = '5353'", "-LocalPort $spec.Port",
		`C:\Program Files\gohere\gohere.exe`, "Get-NetFirewallApplicationFilter", "Get-NetFirewallPortFilter", "Get-NetFirewallAddressFilter",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "Profile Any") || strings.Contains(script, "RemoteAddress Any") {
		t.Fatalf("script contains broad firewall scope:\n%s", script)
	}
}

func TestPowerShellSingleQuoteEscapesExecutablePath(t *testing.T) {
	if got := powerShellQuote(`C:\Users\O'Brien\gohere.exe`); got != `'C:\Users\O''Brien\gohere.exe'` {
		t.Fatalf("quoted path = %q", got)
	}
}
