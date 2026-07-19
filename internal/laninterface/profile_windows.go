//go:build windows

package laninterface

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

func platformNetworkProfile(ctx context.Context, index int) (NetworkProfile, error) {
	script := windowsNetworkProfileScript(index)
	output, err := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script).Output()
	if err != nil {
		return ProfileUnknown, err
	}
	switch strings.TrimSpace(string(output)) {
	case "Private":
		return ProfilePrivate, nil
	case "Public":
		return ProfilePublic, nil
	default:
		return ProfileUnknown, fmt.Errorf("network interface %d is not on a Private Windows profile", index)
	}
}

func windowsNetworkProfileScript(index int) string {
	interfaceIndex := strconv.Itoa(index)
	return "$ErrorActionPreference = 'Stop'; " +
		"$adapter = @(Get-NetAdapter -Physical -InterfaceIndex " + interfaceIndex + " -ErrorAction SilentlyContinue); " +
		"if (@($adapter).Count -ne 1) { throw 'LAN sharing requires a physical network adapter' }; " +
		"(Get-NetConnectionProfile -InterfaceIndex " + interfaceIndex + " -ErrorAction Stop).NetworkCategory.ToString()"
}
