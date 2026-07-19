package app

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/roie/gohere/internal/cli"
	"github.com/roie/gohere/internal/router"
)

func TestRouteReservationCarriesLANShareIntent(t *testing.T) {
	reservation := routeReservationForPlan(cli.Command{Kind: cli.CommandRun, ShareMode: "lan"}, RunPlan{
		Host: "shop.localhost", Name: "shop", URLScheme: "https", RouteTargetHost: "127.0.0.1", Port: 5173, CWD: "/project",
	})
	if reservation.ShareMode != "lan" {
		t.Fatalf("share mode = %q", reservation.ShareMode)
	}
}

func TestCreateAndPrintLANShare(t *testing.T) {
	client := &fakeLANShareClient{result: router.LANShareResult{
		Hostname: "shop.local.", URL: "https://shop.local", SetupURL: "http://192.168.1.42/__gohere/trust/token",
		Fingerprint: "00:01:02:03:04:05:06:07:08:09:0A:0B:0C:0D:0E:0F:10:11:12:13:14:15:16:17:18:19:1A:1B:1C:1D:1E:1F",
	}}
	ref := router.RouteRef{ID: "route-1", Generation: 1}
	var progress bytes.Buffer
	result, err := createLANShare(t.Context(), client, cli.Command{ShareMode: "lan"}, ref, &progress)
	if err != nil {
		t.Fatal(err)
	}
	if progress.String() != "Preparing LAN access…\n" {
		t.Fatalf("progress = %q", progress.String())
	}
	if client.created != ref || result.URL != "https://shop.local" {
		t.Fatalf("created = %#v, result = %#v", client.created, result)
	}
	var output bytes.Buffer
	printLANShare(&output, result)
	wantOutput := "lan    → https://shop.local\nsetup  → http://192.168.1.42/__gohere/trust/token\n\nVerify the certificate fingerprint before trusting:\n00:01:02:03:04:05:06:07:08:09:0A:0B:0C:0D:0E:0F\n10:11:12:13:14:15:16:17:18:19:1A:1B:1C:1D:1E:1F\n\nOnly trust this certificate on devices you control.\n"
	if output.String() != wantOutput {
		t.Fatalf("output = %q", output.String())
	}
	if err := deleteLANShare(t.Context(), client, ref); err != nil {
		t.Fatal(err)
	}
	if client.deleted != ref {
		t.Fatalf("deleted = %#v", client.deleted)
	}
}

func TestCreateLANShareExplainsWindowsFirewallTimeout(t *testing.T) {
	client := &fakeLANShareClient{createErr: errors.New(`companion authority_error: Post "http://127.0.0.1:39399/v2/lan-shares": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`)}
	_, err := createLANShare(t.Context(), client, cli.Command{ShareMode: "lan"}, router.RouteRef{ID: "route-1", Generation: 1}, nil)
	if err == nil || !strings.Contains(err.Error(), "Windows firewall approval") || strings.Contains(err.Error(), "127.0.0.1:39399") {
		t.Fatalf("error = %v", err)
	}
}

type fakeLANShareClient struct {
	result    router.LANShareResult
	createErr error
	created   router.RouteRef
	deleted   router.RouteRef
}

func (*fakeLANShareClient) Health(context.Context) error                    { return nil }
func (*fakeLANShareClient) Routes(context.Context) ([]router.Route, error)  { return nil, nil }
func (*fakeLANShareClient) UpsertRoute(context.Context, router.Route) error { return nil }
func (*fakeLANShareClient) DeleteRoute(context.Context, string) error       { return nil }
func (c *fakeLANShareClient) CreateLANShare(_ context.Context, ref router.RouteRef) (router.LANShareResult, error) {
	c.created = ref
	return c.result, c.createErr
}
func (c *fakeLANShareClient) DeleteLANShare(_ context.Context, ref router.RouteRef) error {
	c.deleted = ref
	return nil
}
