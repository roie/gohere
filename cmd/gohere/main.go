package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/roie/gohere/internal/app"
	"github.com/roie/gohere/internal/cli"
	"github.com/roie/gohere/internal/router"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd, err := cli.Parse(os.Args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	switch cmd.Kind {
	case cli.CommandRun, cli.CommandRaw:
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := app.Run(ctx, cmd, cwd, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case cli.CommandList:
		if err := app.ListWithOptions(ctx, os.Stdout, app.ListOptions{Verbose: cmd.Verbose, JSON: cmd.JSON}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case cli.CommandPrune:
		if err := app.Prune(ctx, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case cli.CommandStop:
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := app.StopWithCommand(ctx, cmd, cwd, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case cli.CommandDoctor:
		if err := app.Doctor(ctx, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case cli.CommandSetup:
		if err := app.Setup(ctx); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case cli.CommandUninstall:
		if err := app.Uninstall(ctx, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case cli.CommandServiceStop:
		if err := app.ServiceStop(ctx, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case cli.CommandServiceRun:
		running, err := router.Start(ctx, router.StartConfig{})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "gohere service listening on %s\n", running.HTTPAddr)
		waitForRouter(ctx, running.Done())
	case cli.CommandHelp:
		printUsage(os.Stdout, cmd.HelpTopic)
	case cli.CommandVersion:
		printVersion(os.Stdout)
	default:
		fmt.Fprintln(os.Stderr, "unknown command")
		os.Exit(2)
	}
}

func waitForRouter(ctx context.Context, done <-chan struct{}) {
	select {
	case <-ctx.Done():
	case <-done:
	}
}

func printUsage(out io.Writer, topic string) {
	if topic != "" {
		if usage, ok := topicUsage(topic); ok {
			fmt.Fprint(out, usage)
			return
		}
		fmt.Fprintf(out, "Usage:\n  gohere %s\n\n", topic)
	}
	fmt.Fprint(out, `Usage:
  gohere [script|file] [script ...] [--as NAME] [--open] [--live] [--verbose] [--target PORT] [--port-flag FLAG]
  gohere --target PORT -- command [args...]
  gohere list [--verbose|--json]
  gohere stop|prune|doctor|service stop|setup|uninstall

Examples:
  gohere
  gohere --open
  gohere dev:web
  gohere dev:web dev:api
  gohere pages/about.html
  gohere --target 5173 -- npm run dev

Flags:
  --open, -o        open the URL in your browser
  --live            auto-refresh static pages when files change
  --as NAME         use NAME.localhost for this run
  --verbose         show target, command, and service details
  --target PORT     use an existing local port
  --port-flag FLAG  override the dev server port flag
`)
}

func topicUsage(topic string) (string, bool) {
	switch topic {
	case "list":
		return `Usage:
  gohere list [--verbose|--json]

Shows active .localhost routes. Use --json for machine-readable output.

`, true
	case "stop":
		return `Usage:
  gohere stop [route|project|--all]

Stops routes by current context, active route/project name, or all safe routes.

`, true
	case "prune":
		return `Usage:
  gohere prune

Removes routes whose targets are confidently dead.

`, true
	case "doctor":
		return `Usage:
  gohere doctor

Checks the gohere service, state files, route status, and platform setup.

`, true
	case "service":
		return `Usage:
  gohere service stop

Stops the background .localhost service without uninstalling gohere.

`, true
	case "setup":
		return `Usage:
  gohere setup

Installs or refreshes the local gohere service.

`, true
	case "uninstall":
		return `Usage:
  gohere uninstall

Removes the local gohere service install and optionally removes local state.

`, true
	default:
		return "", false
	}
}

func printVersion(out io.Writer) {
	fmt.Fprintf(out, "gohere %s\n", version)
}
