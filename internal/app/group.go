package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/roie/gohere/internal/admin"
	"github.com/roie/gohere/internal/router"
	"github.com/roie/gohere/internal/runner"
)

type groupOutput struct {
	stdout *replayWriter
	stderr *replayWriter
}

type groupRunner interface {
	WaitReady() (*runner.Result, error)
	Stop()
}

type launchedGroupRunner struct{ result *runner.Result }

func (r *launchedGroupRunner) WaitReady() (*runner.Result, error) {
	if err := r.result.WaitReady(); err != nil {
		return nil, err
	}
	return r.result, nil
}

func (r *launchedGroupRunner) Stop() { r.result.Stop() }

var launchGroupRunnerFunc = func(ctx context.Context, cfg runner.Config) (groupRunner, error) {
	result, err := runner.Launch(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &launchedGroupRunner{result: result}, nil
}

func runPlannedGroup(ctx context.Context, cmdVerbose bool, items []multiRunItem, client adminClient, stdout, stderr io.Writer) error {
	lifecycleClient, ok := client.(routeLifecycleClient)
	if !ok {
		return errors.New("router control does not support atomic route lifecycle; update gohere")
	}
	seenKeys := map[string]string{}
	for _, item := range items {
		key := serviceDiscoveryEnvKey(serviceDiscoveryLabel(item))
		if key == "" {
			return fmt.Errorf("gohere error: service env key is empty for %s", serviceDiscoverySource(item))
		}
		if existing, exists := seenKeys[key]; exists {
			return fmt.Errorf("gohere error: service env key %q is ambiguous for %s and %s", key, existing, serviceDiscoverySource(item))
		}
		seenKeys[key] = serviceDiscoverySource(item)
	}
	statuses, err := adminRouteStatuses(ctx, client)
	if err != nil {
		if errors.Is(err, admin.ErrUnauthorized) {
			return staleRouterTokenError()
		}
		return err
	}

	runID, err := newRunID()
	if err != nil {
		return err
	}
	request := router.ReservationRequest{RunID: runID, TTL: router.DefaultReservationTTL}
	for i := range items {
		plan := items[i].plan
		candidate := routeReservationForPlan(items[i].cmd, plan)
		reused := false
		if existing, found := reusableExistingRoute(plan, statuses); found && existing.ID != "" && existing.Generation != 0 {
			if !plan.FixedPort || existing.Target == routeTarget(plan.RouteTargetHost, plan.Port) {
				if verifyPlannedTarget(ctx, client, existing.Target, plan.OwnerEnv == "wsl") == nil {
					ref := existing.Ref()
					candidate.Reuse = &ref
					candidate.Target = existing.Target
					candidate.ProjectRoot = ""
					candidate.ProjectName = ""
					reused = true
				}
			}
		}
		if !reused && !plan.ManagedPort && !plan.FixedPort {
			return fmt.Errorf("gohere error: %s does not expose a controllable port; use --target or --port-flag", serviceDiscoverySource(items[i]))
		}
		request.Routes = append(request.Routes, candidate)
	}

	reservation, err := lifecycleClient.ReserveRoutes(ctx, request)
	if err != nil {
		return err
	}
	pendingRefs := reservation.PendingRefs()
	activated := false
	released := false
	release := func() {
		if released || len(pendingRefs) == 0 {
			return
		}
		released = true
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		var err error
		if activated {
			err = deleteRouteRefs(cleanupCtx, lifecycleClient, pendingRefs)
		} else {
			err = lifecycleClient.ReleaseRoutes(cleanupCtx, runID, pendingRefs)
		}
		if err != nil && stderr != nil {
			fmt.Fprintf(stderr, "gohere warning: could not release route group: %v\n", err)
		}
	}
	defer release()
	if len(reservation.Routes) != len(items) {
		return errors.New("router returned an incomplete route reservation batch")
	}

	services := make([]resolvedService, len(items))
	for i, reserved := range reservation.Routes {
		route, err := resolvedRoute(reserved.Route)
		if err != nil {
			return err
		}
		services[i] = resolvedService{
			Plan: items[i].plan, Route: route, Ref: route.Ref(),
			ServiceKey: serviceDiscoveryEnvKey(serviceDiscoveryLabel(items[i])),
			PublicURL:  resolvedPublicURL(items[i].plan, route), Reused: reserved.Reused,
		}
		items[i].reused = reserved.Reused
	}
	for i := range items {
		items[i].plan.Env = resolveServiceEnvironment(items[i].plan.Env, services[i], services)
	}

	outputs := make([]groupOutput, len(items))
	type startResult struct {
		index  int
		result *runner.Result
		err    error
	}
	started := make(chan startResult, len(items))
	launches := make([]groupRunner, len(items))
	newCount := 0
	for i := range items {
		if items[i].reused {
			continue
		}
		newCount++
		label := serviceDiscoveryLabel(items[i])
		outputs[i] = groupOutput{
			stdout: newReplayWriter(32*1024, newLinePrefixWriter(stdout, "["+label+"] ")),
			stderr: newReplayWriter(32*1024, newLinePrefixWriter(stderr, "["+label+"] ")),
		}
		plan := items[i].plan
		launched, err := launchGroupRunnerFunc(ctx, runner.Config{
			Command: plan.Command, Dir: plan.CWD, Env: plan.Env, ChosenPort: plan.Port,
			Stdout: outputs[i].stdout, Stderr: outputs[i].stderr, StartupTimeout: 15 * time.Second,
		})
		if err != nil {
			stopGroupRunners(launches)
			return formatMultiRunError(items[i].cmd, err)
		}
		launches[i] = launched
	}
	defer stopGroupRunners(launches)
	for i, launched := range launches {
		if launched == nil {
			continue
		}
		go func(index int, process groupRunner) {
			result, err := process.WaitReady()
			started <- startResult{index: index, result: result, err: err}
		}(i, launched)
	}
	var firstErr error
	for range newCount {
		startedResult := <-started
		if startedResult.err != nil {
			if firstErr == nil {
				replayGroupStartupOutput(items[startedResult.index], outputs[startedResult.index], stderr)
				firstErr = formatMultiRunError(items[startedResult.index].cmd, startedResult.err)
				stopGroupRunners(launches)
			}
			continue
		}
		items[startedResult.index].result = startedResult.result
		if startedResult.result.Port != items[startedResult.index].plan.Port && firstErr == nil {
			replayGroupStartupOutput(items[startedResult.index], outputs[startedResult.index], stderr)
			firstErr = fmt.Errorf("%s started on port %d, reserved port %d", serviceDiscoverySource(items[startedResult.index]), startedResult.result.Port, items[startedResult.index].plan.Port)
			stopGroupRunners(launches)
		}
	}
	if firstErr != nil {
		return firstErr
	}

	probeErrs := make(chan error, len(items))
	var probeWG sync.WaitGroup
	for i := range items {
		probeWG.Add(1)
		go func(index int) {
			defer probeWG.Done()
			probeErrs <- verifyPlannedTarget(ctx, client, services[index].Route.Target, items[index].plan.OwnerEnv == "wsl")
		}(i)
	}
	probeWG.Wait()
	close(probeErrs)
	for probeErr := range probeErrs {
		if probeErr != nil {
			return probeErr
		}
	}
	if len(pendingRefs) > 0 {
		if _, err := lifecycleClient.ActivateRoutes(ctx, runID, pendingRefs); err != nil {
			return err
		}
		activated = true
	}
	stopLease := startReservationLease(ctx, lifecycleClient, runID, pendingRefs, stderr)
	defer stopLease()

	for i := range items {
		if err := announceResolvedService(ctx, items[i].cmd, services[i], stdout, stderr); err != nil {
			return err
		}
		if items[i].result != nil {
			startLiveOutput(outputs[i].stdout, outputs[i].stderr, cmdVerbose)
		}
	}
	return waitForMulti(ctx, items)
}

func replayGroupStartupOutput(item multiRunItem, output groupOutput, stderr io.Writer) {
	if output.stdout == nil || output.stderr == nil {
		return
	}
	prefix := "[" + serviceDiscoveryLabel(item) + "] "
	replayCapturedOutput(newLinePrefixWriter(stderr, prefix), output.stdout.capture(), output.stderr.capture())
}

func stopGroupRunners(runners []groupRunner) {
	for _, process := range runners {
		if process != nil {
			process.Stop()
		}
	}
}
