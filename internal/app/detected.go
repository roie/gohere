package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/roie/gohere/internal/cli"
	"github.com/roie/gohere/internal/lifecycle"
	"github.com/roie/gohere/internal/router"
)

func deleteRouteRefs(ctx context.Context, client routeLifecycleClient, refs []router.RouteRef) error {
	var errs []error
	for _, ref := range refs {
		if err := client.DeleteRouteRef(ctx, ref); err != nil && !routeRefMismatch(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func routeRefMismatch(err error) bool {
	return errors.Is(err, router.ErrRouteRefMismatch) || strings.Contains(err.Error(), "route identity does not match") || strings.Contains(err.Error(), "409 Conflict")
}

func registerDetectedRouteLifecycle(ctx context.Context, adminClient adminClient, lifecycleClient routeLifecycleClient, cmd cli.Command, plan RunPlan, port, pid int, stdout, stderr io.Writer) (func(), error) {
	plan.Port = port
	processIdentity, _ := lifecycle.ProcessIdentity(pid)
	candidate := routeReservationForPlan(cmd, plan)
	candidate.PID = pid
	candidate.ProcessIdentity = processIdentity
	runID, err := newRunID()
	if err != nil {
		return nil, err
	}
	reservation, err := lifecycleClient.ReserveRoutes(ctx, router.ReservationRequest{
		RunID: runID, TTL: router.DefaultReservationTTL, Routes: []router.RouteReservation{candidate},
	})
	if err != nil {
		return nil, err
	}
	refs := reservation.PendingRefs()
	if len(reservation.Routes) != 1 || len(refs) != 1 {
		_ = lifecycleClient.ReleaseRoutes(ctx, runID, refs)
		return nil, fmt.Errorf("router returned an incomplete detected route reservation")
	}
	route, err := resolvedRoute(reservation.Routes[0].Route)
	if err != nil {
		_ = lifecycleClient.ReleaseRoutes(ctx, runID, refs)
		return nil, err
	}
	if err := verifyPlannedTarget(ctx, adminClient, route.Target, plan.OwnerEnv == "wsl"); err != nil {
		_ = lifecycleClient.ReleaseRoutes(ctx, runID, refs)
		if plan.OwnerEnv == "wsl" {
			return nil, fmt.Errorf("Windows cannot reach WSL target %s: %w", route.Target, err)
		}
		return nil, err
	}
	active, err := lifecycleClient.ActivateRoutes(ctx, runID, refs)
	if err != nil {
		_ = lifecycleClient.ReleaseRoutes(ctx, runID, refs)
		return nil, err
	}
	if len(active) == 1 {
		route = active[0]
	}
	plan.Host = route.Host
	plan.Name = route.Name
	service := resolvedService{Plan: plan, Route: route, Ref: route.Ref(), ServiceKey: serviceDiscoveryEnvKey(plan.Name), PublicURL: resolvedPublicURL(plan, route)}
	stopLease := startReservationLease(ctx, lifecycleClient, runID, refs, stderr)
	lanShare, err := createLANShare(ctx, adminClient, cmd, route.Ref())
	if err != nil {
		stopLease()
		_ = deleteRouteRefs(ctx, lifecycleClient, refs)
		return nil, err
	}
	if err := announceResolvedService(ctx, cmd, service, stdout, stderr); err != nil {
		stopLease()
		if lanShare != nil {
			_ = deleteLANShare(ctx, adminClient, route.Ref())
		}
		_ = deleteRouteRefs(ctx, lifecycleClient, refs)
		return nil, err
	}
	printLANShare(stdout, lanShare)
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			stopLease()
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if lanShare != nil {
				if err := deleteLANShare(cleanupCtx, adminClient, route.Ref()); err != nil && stderr != nil {
					fmt.Fprintln(stderr, routeCleanupWarning(route.Host, err))
				}
			}
			if err := deleteRouteRefs(cleanupCtx, lifecycleClient, refs); err != nil && stderr != nil {
				fmt.Fprintln(stderr, routeCleanupWarning(route.Host, err))
			}
		})
	}
	stopContextCleanup := context.AfterFunc(ctx, cleanup)
	return func() {
		stopContextCleanup()
		cleanup()
	}, nil
}
