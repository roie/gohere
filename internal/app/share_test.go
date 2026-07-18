package app

import (
	"io"
	"testing"

	"github.com/roie/gohere/internal/cli"
)

func TestRouteReservationCarriesLANShareIntent(t *testing.T) {
	reservation := routeReservationForPlan(cli.Command{Kind: cli.CommandRun, ShareMode: "lan"}, RunPlan{
		Host: "shop.localhost", Name: "shop", URLScheme: "https", RouteTargetHost: "127.0.0.1", Port: 5173, CWD: "/project",
	})
	if reservation.ShareMode != "lan" {
		t.Fatalf("share mode = %q", reservation.ShareMode)
	}
}

func TestRunRejectsLANShareUntilAuthorityIntegration(t *testing.T) {
	err := Run(t.Context(), cli.Command{Kind: cli.CommandRun, ShareMode: "lan"}, t.TempDir(), io.Discard, io.Discard)
	if err == nil || err.Error() != "gohere error: LAN sharing is not available yet" {
		t.Fatalf("Run() error = %v", err)
	}
}
