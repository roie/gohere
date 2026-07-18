package app

import (
	"io"
	"testing"

	"github.com/roie/gohere/internal/cli"
)

func TestRunRejectsLANShareUntilAuthorityIntegration(t *testing.T) {
	err := Run(t.Context(), cli.Command{Kind: cli.CommandRun, ShareMode: "lan"}, t.TempDir(), io.Discard, io.Discard)
	if err == nil || err.Error() != "gohere error: LAN sharing is not available yet" {
		t.Fatalf("Run() error = %v", err)
	}
}
