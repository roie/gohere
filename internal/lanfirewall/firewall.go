package lanfirewall

import "strings"

func windowsFirewallScript(executable string) string {
	program := powerShellQuote(executable)
	return `$ErrorActionPreference = 'Stop'
$program = ` + program + `
$rules = @(
  @{ Name = 'gohere LAN HTTPS'; Protocol = 'TCP'; Port = '443' },
  @{ Name = 'gohere LAN onboarding'; Protocol = 'TCP'; Port = '80' },
  @{ Name = 'gohere LAN mDNS'; Protocol = 'UDP'; Port = '5353' }
)
foreach ($spec in $rules) {
  $rule = Get-NetFirewallRule -DisplayName $spec.Name -ErrorAction SilentlyContinue
  if (-not $rule) {
    New-NetFirewallRule -DisplayName $spec.Name -Program $program -Direction Inbound -Action Allow -Enabled True -Profile Private -Protocol $spec.Protocol -LocalPort $spec.Port -RemoteAddress LocalSubnet | Out-Null
    continue
  }
  if (@($rule).Count -ne 1) { throw "Firewall rule '$($spec.Name)' is ambiguous" }
  $application = $rule | Get-NetFirewallApplicationFilter
  $port = $rule | Get-NetFirewallPortFilter
  $address = $rule | Get-NetFirewallAddressFilter
  if ($rule.Direction -ne 'Inbound' -or $rule.Action -ne 'Allow' -or $rule.Enabled -ne 'True' -or $rule.Profile -ne 'Private' -or
      $application.Program -ne $program -or $port.Protocol -ne $spec.Protocol -or $port.LocalPort -ne $spec.Port -or
      $address.RemoteAddress -notcontains 'LocalSubnet') {
    throw "Existing firewall rule '$($spec.Name)' is not owned by gohere with the required private-network scope"
  }
}
`
}

func powerShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
