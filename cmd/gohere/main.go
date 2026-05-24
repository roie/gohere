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
		if err := app.List(ctx, os.Stdout, cmd.Verbose); err != nil {
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
		if err := app.Stop(ctx, cwd, os.Stdout); err != nil {
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
		fmt.Fprintf(out, "Usage: gohere %s\n\n", topic)
	}
	fmt.Fprint(out, `Usage:
  gohere [script|file] [script ...] [--as NAME] [--open] [--verbose] [--target PORT] [--port-flag FLAG]
  gohere --target PORT -- command [args...]
  gohere list|stop|prune|doctor|service stop|setup|uninstall

Examples:
  gohere
  gohere --open
  gohere dev:web
  gohere dev:web dev:api
  gohere pages/about.html
  gohere --target 5173 -- npm run dev

Flags:
  --open, -o        open the URL in your browser
  --as NAME         use NAME.localhost for this run
  --verbose         show target, command, and service details
  --target PORT     use an existing local port
  --port-flag FLAG  override the dev server port flag
`)
}

func printVersion(out io.Writer) {
	fmt.Fprintf(out, "gohere %s\n", version)
}
