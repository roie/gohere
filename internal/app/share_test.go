package app

import (
	"bytes"
	"context"
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
	client := &fakeLANShareClient{result: router.LANShareResult{Hostname: "shop.local.", URL: "https://shop.local"}}
	ref := router.RouteRef{ID: "route-1", Generation: 1}
	result, err := createLANShare(t.Context(), client, cli.Command{ShareMode: "lan"}, ref)
	if err != nil {
		t.Fatal(err)
	}
	if client.created != ref || result.URL != "https://shop.local" {
		t.Fatalf("created = %#v, result = %#v", client.created, result)
	}
	var output bytes.Buffer
	printLANShare(&output, result)
	if output.String() != "share  → https://shop.local\n" {
		t.Fatalf("output = %q", output.String())
	}
	if err := deleteLANShare(t.Context(), client, ref); err != nil {
		t.Fatal(err)
	}
	if client.deleted != ref {
		t.Fatalf("deleted = %#v", client.deleted)
	}
}

type fakeLANShareClient struct {
	result  router.LANShareResult
	created router.RouteRef
	deleted router.RouteRef
}

func (*fakeLANShareClient) Health(context.Context) error                    { return nil }
func (*fakeLANShareClient) Routes(context.Context) ([]router.Route, error)  { return nil, nil }
func (*fakeLANShareClient) UpsertRoute(context.Context, router.Route) error { return nil }
func (*fakeLANShareClient) DeleteRoute(context.Context, string) error       { return nil }
func (c *fakeLANShareClient) CreateLANShare(_ context.Context, ref router.RouteRef) (router.LANShareResult, error) {
	c.created = ref
	return c.result, nil
}
func (c *fakeLANShareClient) DeleteLANShare(_ context.Context, ref router.RouteRef) error {
	c.deleted = ref
	return nil
}
