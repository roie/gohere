package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"

	"github.com/roie/gohere/internal/cli"
	"github.com/roie/gohere/internal/router"
)

type runIntent uint8

const (
	runIntentLazy runIntent = iota
	runIntentPlanned
)

type routeLifecycleClient interface {
	ReserveRoutes(context.Context, router.ReservationRequest) (router.ReservationResult, error)
	ActivateRoutes(context.Context, string, []router.RouteRef) ([]router.Route, error)
	ReleaseRoutes(context.Context, string, []router.RouteRef) error
	RenewRoutes(context.Context, string, []router.RouteRef) error
	DeleteRouteRef(context.Context, router.RouteRef) error
}

type resolvedService struct {
	Plan       RunPlan
	Route      router.Route
	Ref        router.RouteRef
	ServiceKey string
	PublicURL  string
	Reused     bool
}

func classifyRunIntent(cmd cli.Command, plan RunPlan) runIntent {
	if plan.Static || plan.Mode == "workspace" || plan.Mode == "multi" || cmd.TargetPath != "" || cmd.Live {
		return runIntentPlanned
	}
	if cmd.TargetPort > 0 || strings.TrimSpace(cmd.PortFlag) != "" {
		return runIntentPlanned
	}
	if cmd.Kind == cli.CommandRaw {
		return runIntentLazy
	}
	if cmd.Kind != cli.CommandRun {
		return runIntentLazy
	}
	if len(cmd.Scripts) > 1 || (!cmd.ExplicitScript && cmd.Script == "dev") {
		return runIntentPlanned
	}
	firstSegment := strings.ToLower(strings.SplitN(cmd.Script, ":", 2)[0])
	switch firstSegment {
	case "build", "lint", "test":
		return runIntentLazy
	case "dev", "start", "serve", "preview":
		return runIntentPlanned
	}
	if plan.ManagedPort {
		return runIntentPlanned
	}
	return runIntentLazy
}

func resolveServiceEnvironment(base []string, current resolvedService, all []resolvedService) []string {
	env := make([]string, 0, len(base)+3+len(all)*3)
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || isGeneratedRuntimeKey(key) {
			continue
		}
		env = append(env, entry)
	}

	host := strings.TrimSpace(current.Plan.ListenHost)
	if host == "" {
		host = "127.0.0.1"
	}
	env = append(env, "HOST="+host)
	if port, ok := routeTargetPort(current.Route.Target); ok {
		env = append(env, "PORT="+port)
	}
	env = append(env, "GOHERE_URL="+current.PublicURL)

	if len(all) > 1 {
		for _, service := range all {
			key := serviceDiscoveryEnvKey(service.ServiceKey)
			if key == "" {
				continue
			}
			prefix := "GOHERE_" + key + "_"
			env = append(env, prefix+"URL="+service.PublicURL)
			env = append(env, prefix+"TARGET="+service.Route.Target)
			if port, ok := routeTargetPort(service.Route.Target); ok {
				env = append(env, prefix+"PORT="+port)
			}
		}
	}
	return env
}

func newRunID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(random[:]), nil
}

func resolvedRoute(route router.Route) (router.Route, error) {
	if route.EffectiveState() == router.RouteStatePending {
		if strings.TrimSpace(route.PendingTarget) == "" {
			return router.Route{}, errors.New("pending route omitted target")
		}
		route.Target = route.PendingTarget
	}
	return route, nil
}

func resolvedPublicURL(plan RunPlan, route router.Route) string {
	return publicRouteURLForScheme(plan.URLScheme, route.Host, plan.URLPath)
}

func routeTargetPort(target string) (string, bool) {
	parsed, err := url.Parse(target)
	if err != nil || parsed.Scheme == "" || parsed.Hostname() == "" || parsed.Port() == "" {
		return "", false
	}
	return parsed.Port(), true
}

func isGeneratedRuntimeKey(key string) bool {
	if key == "HOST" || key == "PORT" || key == "GOHERE_URL" || key == "GOHERE_SERVICES_JSON" {
		return true
	}
	if !strings.HasPrefix(key, "GOHERE_") {
		return false
	}
	for _, suffix := range []string{"_URL", "_TARGET", "_PORT"} {
		if middle, ok := strings.CutSuffix(strings.TrimPrefix(key, "GOHERE_"), suffix); ok && middle != "" {
			return true
		}
	}
	return false
}
