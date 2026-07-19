package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/roie/gohere/internal/cli"
	"github.com/roie/gohere/internal/router"
)

type lanShareClient interface {
	CreateLANShare(context.Context, router.RouteRef) (router.LANShareResult, error)
	DeleteLANShare(context.Context, router.RouteRef) error
}

func createLANShare(ctx context.Context, client adminClient, cmd cli.Command, ref router.RouteRef, progress io.Writer) (*router.LANShareResult, error) {
	if cmd.ShareMode == "" {
		return nil, nil
	}
	if cmd.ShareMode != "lan" {
		return nil, fmt.Errorf("unsupported share mode %q", cmd.ShareMode)
	}
	lanClient, ok := client.(lanShareClient)
	if !ok {
		return nil, errors.New("router control does not support LAN sharing; update gohere")
	}
	finishProgress := startLANProgress(progress, isTerminalOutput(progress))
	defer finishProgress()
	result, err := lanClient.CreateLANShare(ctx, ref)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "Client.Timeout exceeded") {
			return nil, errors.New("gohere error: LAN setup timed out while waiting for Windows firewall approval; approve the Windows prompt, then run the command again")
		}
		return nil, err
	}
	return &result, nil
}

func startLANProgress(output io.Writer, terminal bool) func() {
	if output == nil || !terminal {
		return func() {}
	}
	const message = "Preparing LAN access…"
	_, _ = fmt.Fprint(output, message)
	return func() { _, _ = fmt.Fprintf(output, "\r%s\r", strings.Repeat(" ", len([]rune(message)))) }
}

func deleteLANShare(ctx context.Context, client adminClient, ref router.RouteRef) error {
	lanClient, ok := client.(lanShareClient)
	if !ok {
		return errors.New("router control does not support LAN sharing; update gohere")
	}
	return lanClient.DeleteLANShare(ctx, ref)
}

func lanShareNoServerError(cmd cli.Command) error {
	return fmt.Errorf("gohere error: %s did not open a server, so LAN sharing was not started", runName(cmd))
}

func printLANShare(output io.Writer, result *router.LANShareResult) {
	if result == nil {
		return
	}
	fmt.Fprintf(output, "lan    → %s\n", result.URL)
	if result.SetupURL != "" {
		fmt.Fprintf(output, "setup  → %s\n", result.SetupURL)
	}
	printedQR := maybePrintLANSetupQR(output, result.SetupURL)
	if result.Fingerprint != "" {
		if !printedQR {
			fmt.Fprintln(output)
		}
		fmt.Fprintln(output, "Verify the certificate fingerprint before trusting:")
		fmt.Fprintln(output, wrapLANFingerprint(result.Fingerprint, 16))
		fmt.Fprintln(output)
		fmt.Fprintln(output, "Only trust this certificate on devices you control.")
	}
}

func wrapLANFingerprint(fingerprint string, bytesPerLine int) string {
	parts := strings.Split(fingerprint, ":")
	if bytesPerLine <= 0 || len(parts) <= bytesPerLine {
		return fingerprint
	}
	lines := make([]string, 0, (len(parts)+bytesPerLine-1)/bytesPerLine)
	for len(parts) > 0 {
		count := min(bytesPerLine, len(parts))
		lines = append(lines, strings.Join(parts[:count], ":"))
		parts = parts[count:]
	}
	return strings.Join(lines, "\n")
}
