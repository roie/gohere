package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/roie/gohere/internal/admin"
	"github.com/roie/gohere/internal/cli"
	"github.com/roie/gohere/internal/router"
	"github.com/roie/gohere/internal/runner"
	"github.com/roie/gohere/internal/staticserver"
)

func runPlannedSingle(ctx context.Context, cmd cli.Command, plan RunPlan, client adminClient, stdout, stderr io.Writer) error {
	lifecycleClient, ok := client.(routeLifecycleClient)
	if !ok {
		return errors.New("router control does not support atomic route lifecycle; update gohere")
	}
	runID, err := newRunID()
	if err != nil {
		return err
	}
	candidate := routeReservationForPlan(cmd, plan)
	if reusable, ok, err := plannedReusableRoute(ctx, client, plan); err != nil {
		return err
	} else if ok {
		ref := reusable.Ref()
		candidate.Reuse = &ref
	}
	reservation, err := lifecycleClient.ReserveRoutes(ctx, router.ReservationRequest{RunID: runID, TTL: router.DefaultReservationTTL, Routes: []router.RouteReservation{candidate}})
	if err != nil {
		if errors.Is(err, admin.ErrUnauthorized) {
			return staleRouterTokenError()
		}
		return err
	}
	if len(reservation.Routes) != 1 {
		return errors.New("router returned an incomplete route reservation")
	}
	reserved := reservation.Routes[0]
	route, err := resolvedRoute(reserved.Route)
	if err != nil {
		return releaseReservationError(ctx, lifecycleClient, runID, reservation.PendingRefs(), err)
	}
	service := resolvedService{Plan: plan, Route: route, Ref: route.Ref(), ServiceKey: serviceDiscoveryEnvKey(plan.Name), PublicURL: resolvedPublicURL(plan, route), Reused: reserved.Reused}
	pendingRefs := reservation.PendingRefs()
	activated := false
	released := false
	var lanShare *router.LANShareResult
	release := func() {
		if released || len(pendingRefs) == 0 {
			return
		}
		released = true
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		var err error
		if lanShare != nil {
			err = deleteLANShare(cleanupCtx, client, route.Ref())
		}
		if activated {
			err = errors.Join(err, deleteRouteRefs(cleanupCtx, lifecycleClient, pendingRefs))
		} else {
			err = errors.Join(err, lifecycleClient.ReleaseRoutes(cleanupCtx, runID, pendingRefs))
		}
		if err != nil && stderr != nil {
			fmt.Fprintln(stderr, routeCleanupWarning(route.Host, err))
		}
	}
	defer release()

	if service.Reused {
		if err := verifyPlannedTarget(ctx, client, route.Target, plan.OwnerEnv == "wsl"); err != nil {
			return err
		}
		lanShare, err = createLANShare(ctx, client, cmd, route.Ref())
		if err != nil {
			return err
		}
		if err := announceResolvedService(ctx, cmd, service, stdout, stderr); err != nil {
			return err
		}
		printLANShare(stdout, lanShare)
		return nil
	}

	if plan.Static {
		staticServer, err := staticserver.StartWithConfig(ctx, staticserver.Config{Dir: plan.CWD, Port: plan.Port, Host: plan.StaticBindHost, Live: plan.Live})
		if err != nil {
			return err
		}
		defer staticServer.Close()
		if staticServer.Port() != plan.Port {
			return fmt.Errorf("static server started on port %d, reserved port %d", staticServer.Port(), plan.Port)
		}
		if err := verifyPlannedTarget(ctx, client, route.Target, plan.OwnerEnv == "wsl"); err != nil {
			return err
		}
		if _, err := lifecycleClient.ActivateRoutes(ctx, runID, pendingRefs); err != nil {
			return err
		}
		activated = true
		stopLease := startReservationLease(ctx, lifecycleClient, runID, pendingRefs, stderr)
		defer stopLease()
		lanShare, err = createLANShare(ctx, client, cmd, route.Ref())
		if err != nil {
			return err
		}
		if err := announceResolvedService(ctx, cmd, service, stdout, stderr); err != nil {
			return err
		}
		printLANShare(stdout, lanShare)
		<-ctx.Done()
		return nil
	}

	plan.Env = resolveServiceEnvironment(plan.Env, service, []resolvedService{service})
	liveStdout := newReplayWriter(32*1024, stdout)
	liveStderr := newReplayWriter(32*1024, stderr)
	result, err := startRunnerFunc(ctx, runner.Config{Command: plan.Command, Dir: runnerDirForRun(cmd, plan), Env: plan.Env, ChosenPort: plan.Port, RequireDetectedPort: false, Stdout: liveStdout, Stderr: liveStderr, StartupTimeout: 15 * time.Second})
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		replayCapturedOutput(stderr, liveStdout.capture(), liveStderr.capture())
		if errors.Is(err, runner.ErrProcessFinished) {
			fmt.Fprint(stdout, runFinishedOutput(cmd))
			return nil
		}
		return formatRunError(cmd, err)
	}
	defer result.Stop()
	if result.Port != plan.Port {
		return fmt.Errorf("server started on port %d, reserved port %d", result.Port, plan.Port)
	}
	if err := verifyPlannedTarget(ctx, client, route.Target, plan.OwnerEnv == "wsl"); err != nil {
		return err
	}
	if _, err := lifecycleClient.ActivateRoutes(ctx, runID, pendingRefs); err != nil {
		return err
	}
	activated = true
	stopLease := startReservationLease(ctx, lifecycleClient, runID, pendingRefs, stderr)
	defer stopLease()
	lanShare, err = createLANShare(ctx, client, cmd, route.Ref())
	if err != nil {
		return err
	}
	if err := announceResolvedService(ctx, cmd, service, stdout, stderr); err != nil {
		return err
	}
	printLANShare(stdout, lanShare)
	startLiveOutput(liveStdout, liveStderr, cmd.Verbose)
	return result.Wait()
}

func routeReservationForPlan(cmd cli.Command, plan RunPlan) router.RouteReservation {
	ownerEnv := plan.OwnerEnv
	if ownerEnv == "" {
		ownerEnv = runOwnerEnv()
	}
	return router.RouteReservation{
		DesiredHost: plan.Host, Service: plan.Name, PreferredScheme: plan.URLScheme, URLPath: plan.URLPath,
		Target: routeTarget(plan.RouteTargetHost, plan.Port), CWD: plan.CWD,
		ProjectRoot: plan.ProjectRoot, ProjectName: plan.ProjectName, Source: plan.RouteSource,
		OwnerCWD: plan.CWD, OwnerEnv: ownerEnv, OwnerInstance: plan.OwnerInstance,
		Distro: plan.Distro, LinuxUser: plan.LinuxUser, RunnerID: plan.RunnerID,
		Mode: runMode(cmd, plan), ShareMode: cmd.ShareMode, ListenTarget: routeTarget(plan.ListenHost, plan.Port),
	}
}

func plannedReusableRoute(ctx context.Context, client adminClient, plan RunPlan) (router.Route, bool, error) {
	statuses, err := adminRouteStatuses(ctx, client)
	if err != nil {
		if errors.Is(err, admin.ErrUnauthorized) {
			return router.Route{}, false, staleRouterTokenError()
		}
		return router.Route{}, false, err
	}
	route, ok := reusableExistingRoute(plan, statuses)
	if !ok || (plan.FixedPort && route.Target != routeTarget(plan.RouteTargetHost, plan.Port)) {
		return router.Route{}, false, nil
	}
	if err := verifyPlannedTarget(ctx, client, route.Target, plan.OwnerEnv == "wsl"); err != nil {
		return router.Route{}, false, nil
	}
	return route, true, nil
}

func verifyPlannedTarget(ctx context.Context, client adminClient, target string, retry bool) error {
	probeClient, ok := client.(bridgeProbeClient)
	if !ok {
		return nil
	}
	if retry {
		reachable, err := waitForWSLTarget(ctx, probeClient, target)
		if err != nil {
			return err
		}
		if !reachable {
			return fmt.Errorf("router cannot reach target %s", target)
		}
		return nil
	}
	reachable, err := probeClient.ProbeTarget(ctx, target)
	if err != nil {
		return err
	}
	if !reachable {
		return fmt.Errorf("router cannot reach target %s", target)
	}
	return nil
}

func announceResolvedService(ctx context.Context, cmd cli.Command, service resolvedService, stdout, stderr io.Writer) error {
	fmt.Fprint(stdout, runSuccessOutputForScheme(cmd, service.Plan.URLScheme, service.Route.Host, service.Plan.URLPath))
	if cmd.Open {
		if err := openBrowserFunc(ctx, service.PublicURL); err != nil {
			fmt.Fprintf(stderr, "Could not open browser automatically.\nOpen manually: %s\n", service.PublicURL)
		}
	}
	if cmd.Verbose {
		fmt.Fprintf(stdout, "\ntarget: %s\n", service.Route.Target)
		if service.Plan.ProjectRoot != "" {
			fmt.Fprintf(stdout, "project root: %s\n", service.Plan.ProjectRoot)
		}
		fmt.Fprintf(stdout, "command: %s\n", strings.Join(service.Plan.Command, " "))
		routerLabel := service.Plan.RouterLabel
		if routerLabel == "" {
			routerLabel = "running"
		}
		fmt.Fprintf(stdout, "service: %s\n", routerLabel)
	}
	return nil
}

func releaseReservationError(ctx context.Context, client routeLifecycleClient, runID string, refs []router.RouteRef, cause error) error {
	if len(refs) == 0 {
		return cause
	}
	if err := client.ReleaseRoutes(ctx, runID, refs); err != nil {
		return fmt.Errorf("%w (reservation rollback failed: %v)", cause, err)
	}
	return cause
}

func startReservationLease(ctx context.Context, client routeLifecycleClient, runID string, refs []router.RouteRef, stderr io.Writer) func() {
	leaseCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(done)
		ticker := time.NewTicker(routeLeaseInterval)
		defer ticker.Stop()
		for {
			select {
			case <-leaseCtx.Done():
				return
			case <-ticker.C:
				var renewErrors []error
				for _, ref := range refs {
					if err := client.RenewRoutes(leaseCtx, runID, []router.RouteRef{ref}); err != nil && !routeRefMismatch(err) {
						renewErrors = append(renewErrors, err)
					}
				}
				if err := errors.Join(renewErrors...); err != nil && stderr != nil {
					fmt.Fprintf(stderr, "gohere warning: could not renew route lease: %v\n", err)
				}
			}
		}
	}()
	return func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}
