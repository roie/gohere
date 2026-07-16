package app

import (
	"reflect"
	"strings"
	"testing"

	"github.com/roie/gohere/internal/cli"
	"github.com/roie/gohere/internal/router"
)

func TestClassifyRunIntent(t *testing.T) {
	tests := []struct {
		name string
		cmd  cli.Command
		plan RunPlan
		want runIntent
	}{
		{name: "default", cmd: cli.Command{Kind: cli.CommandRun, Script: "dev"}, want: runIntentPlanned},
		{name: "static", cmd: cli.Command{Kind: cli.CommandRun, ExplicitScript: true, Script: "anything"}, plan: RunPlan{Static: true}, want: runIntentPlanned},
		{name: "workspace", cmd: cli.Command{Kind: cli.CommandRun, ExplicitScript: true, Script: "build"}, plan: RunPlan{Mode: "workspace"}, want: runIntentPlanned},
		{name: "multi", cmd: cli.Command{Kind: cli.CommandRun, ExplicitScript: true, Script: "web", Scripts: []string{"web", "api"}}, want: runIntentPlanned},
		{name: "dev segment", cmd: cli.Command{Kind: cli.CommandRun, ExplicitScript: true, Script: "dev:web"}, want: runIntentPlanned},
		{name: "start segment", cmd: cli.Command{Kind: cli.CommandRun, ExplicitScript: true, Script: "start:api"}, want: runIntentPlanned},
		{name: "serve segment", cmd: cli.Command{Kind: cli.CommandRun, ExplicitScript: true, Script: "serve:docs"}, want: runIntentPlanned},
		{name: "preview segment", cmd: cli.Command{Kind: cli.CommandRun, ExplicitScript: true, Script: "preview:site"}, want: runIntentPlanned},
		{name: "managed adapter", cmd: cli.Command{Kind: cli.CommandRun, ExplicitScript: true, Script: "custom"}, plan: RunPlan{ManagedPort: true}, want: runIntentPlanned},
		{name: "target", cmd: cli.Command{Kind: cli.CommandRun, ExplicitScript: true, Script: "custom", TargetPort: 4100}, want: runIntentPlanned},
		{name: "port flag", cmd: cli.Command{Kind: cli.CommandRun, ExplicitScript: true, Script: "custom", PortFlag: "--listen"}, want: runIntentPlanned},
		{name: "path", cmd: cli.Command{Kind: cli.CommandRun, TargetPath: "./site"}, want: runIntentPlanned},
		{name: "build", cmd: cli.Command{Kind: cli.CommandRun, ExplicitScript: true, Script: "build"}, want: runIntentLazy},
		{name: "lint", cmd: cli.Command{Kind: cli.CommandRun, ExplicitScript: true, Script: "lint"}, want: runIntentLazy},
		{name: "test", cmd: cli.Command{Kind: cli.CommandRun, ExplicitScript: true, Script: "test"}, want: runIntentLazy},
		{name: "raw", cmd: cli.Command{Kind: cli.CommandRaw, Raw: []string{"node", "server.js"}}, want: runIntentLazy},
		{name: "raw target", cmd: cli.Command{Kind: cli.CommandRaw, Raw: []string{"node", "server.js"}, TargetPort: 4100}, want: runIntentPlanned},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyRunIntent(test.cmd, test.plan); got != test.want {
				t.Fatalf("intent = %v, want %v", got, test.want)
			}
		})
	}
}

func TestResolveServiceEnvironmentUsesReservedRecords(t *testing.T) {
	web := resolvedService{
		Plan:  RunPlan{ListenHost: "127.0.0.1"},
		Route: router.Route{Host: "web-2.localhost", Target: "http://172.22.0.5:48101"},
		Ref:   router.RouteRef{ID: "web-route", Generation: 2}, ServiceKey: "WEB", PublicURL: "https://web-2.localhost", Reused: true,
	}
	api := resolvedService{
		Plan:  RunPlan{ListenHost: "0.0.0.0"},
		Route: router.Route{Host: "api.localhost", Target: "http://172.22.0.5:48102"},
		Ref:   router.RouteRef{ID: "api-route", Generation: 1}, ServiceKey: "API", PublicURL: "http://api.localhost",
	}
	base := []string{
		"PATH=/bin", "CUSTOM=keep", "HOST=stale", "PORT=9999", "GOHERE_URL=https://stale.localhost",
		"GOHERE_WEB_URL=https://old.localhost", "GOHERE_OLD_TARGET=http://127.0.0.1:1", "GOHERE_OLD_PORT=1", "GOHERE_SERVICES_JSON={}",
	}
	all := []resolvedService{web, api}
	webEnv := envValues(resolveServiceEnvironment(base, web, all))
	apiEnv := envValues(resolveServiceEnvironment(base, api, all))

	for _, env := range []map[string]string{webEnv, apiEnv} {
		wantNamed := map[string]string{
			"GOHERE_WEB_URL": "https://web-2.localhost", "GOHERE_WEB_TARGET": "http://172.22.0.5:48101", "GOHERE_WEB_PORT": "48101",
			"GOHERE_API_URL": "http://api.localhost", "GOHERE_API_TARGET": "http://172.22.0.5:48102", "GOHERE_API_PORT": "48102",
		}
		for key, want := range wantNamed {
			if env[key] != want {
				t.Fatalf("%s = %q, want %q in %#v", key, env[key], want, env)
			}
		}
		if env["CUSTOM"] != "keep" || env["PATH"] != "/bin" {
			t.Fatalf("unrelated environment not preserved: %#v", env)
		}
		for _, stale := range []string{"GOHERE_OLD_TARGET", "GOHERE_OLD_PORT", "GOHERE_SERVICES_JSON"} {
			if _, ok := env[stale]; ok {
				t.Fatalf("stale %s survived: %#v", stale, env)
			}
		}
	}
	if webEnv["HOST"] != "127.0.0.1" || webEnv["PORT"] != "48101" || webEnv["GOHERE_URL"] != web.PublicURL {
		t.Fatalf("web self environment = %#v", webEnv)
	}
	if apiEnv["HOST"] != "0.0.0.0" || apiEnv["PORT"] != "48102" || apiEnv["GOHERE_URL"] != api.PublicURL {
		t.Fatalf("api self environment = %#v", apiEnv)
	}
	for key, value := range webEnv {
		if isNamedDiscoveryKey(key) && apiEnv[key] != value {
			t.Fatalf("named map differs for %s: web=%q api=%q", key, value, apiEnv[key])
		}
	}
}

func TestResolveServiceEnvironmentSingleHasNoNamedMap(t *testing.T) {
	service := resolvedService{Plan: RunPlan{ListenHost: "127.0.0.1"}, Route: router.Route{Host: "site.localhost", Target: "http://127.0.0.1:48201"}, ServiceKey: "SITE", PublicURL: "https://site.localhost"}
	env := envValues(resolveServiceEnvironment([]string{"OTHER=value", "GOHERE_API_URL=stale"}, service, []resolvedService{service}))
	want := map[string]string{"OTHER": "value", "HOST": "127.0.0.1", "PORT": "48201", "GOHERE_URL": "https://site.localhost"}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("environment = %#v, want %#v", env, want)
	}
}

func TestResolvedPublicURLUsesReservedHostnameAndPreferredScheme(t *testing.T) {
	plan := RunPlan{URLScheme: "https", URLPath: "/docs/"}
	if got := resolvedPublicURL(plan, router.Route{Host: "docs-3.localhost"}); got != "https://docs-3.localhost/docs/" {
		t.Fatalf("URL = %q", got)
	}
}

func envValues(env []string) map[string]string {
	values := make(map[string]string, len(env))
	for _, value := range env {
		for i := range value {
			if value[i] == '=' {
				values[value[:i]] = value[i+1:]
				break
			}
		}
	}
	return values
}

func isNamedDiscoveryKey(key string) bool {
	if !strings.HasPrefix(key, "GOHERE_") || key == "GOHERE_URL" {
		return false
	}
	return strings.HasSuffix(key, "_URL") || strings.HasSuffix(key, "_TARGET") || strings.HasSuffix(key, "_PORT")
}
