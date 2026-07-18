package app

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/roie/gohere/internal/cli"
	"github.com/roie/gohere/internal/router"
)

type lanShareClient interface {
	CreateLANShare(context.Context, router.RouteRef) (router.LANShareResult, error)
	DeleteLANShare(context.Context, router.RouteRef) error
}

func createLANShare(ctx context.Context, client adminClient, cmd cli.Command, ref router.RouteRef) (*router.LANShareResult, error) {
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
	result, err := lanClient.CreateLANShare(ctx, ref)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func deleteLANShare(ctx context.Context, client adminClient, ref router.RouteRef) error {
	lanClient, ok := client.(lanShareClient)
	if !ok {
		return errors.New("router control does not support LAN sharing; update gohere")
	}
	return lanClient.DeleteLANShare(ctx, ref)
}

func printLANShare(output io.Writer, result *router.LANShareResult) {
	if result == nil {
		return
	}
	fmt.Fprintf(output, "share  → %s\n", result.URL)
	if result.SetupURL != "" {
		fmt.Fprintf(output, "setup  → %s\n", result.SetupURL)
	}
	if result.Fingerprint != "" {
		fmt.Fprintf(output, "CA fingerprint: %s\n", result.Fingerprint)
		fmt.Fprintln(output, "Only install this certificate on devices you control. Verify the fingerprint before enabling trust.")
	}
	maybePrintLANSetupQR(output, result.SetupURL)
}
