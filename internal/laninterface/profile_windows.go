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
	script := "(Get-NetConnectionProfile -InterfaceIndex " + strconv.Itoa(index) + " -ErrorAction Stop).NetworkCategory.ToString()"
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
