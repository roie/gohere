package main

import (
	"context"
	"fmt"
	"os"

	"github.com/roie/gohere/internal/app"
	"github.com/roie/gohere/internal/cli"
	"github.com/roie/gohere/internal/router"
)

func main() {
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
		if err := app.Run(context.Background(), cmd, cwd, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case cli.CommandRouter:
		running, err := router.Start(context.Background(), router.StartConfig{})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "gohere router listening on %s\n", running.HTTPAddr)
		select {}
	case cli.CommandList, cli.CommandStop, cli.CommandClean, cli.CommandDoctor, cli.CommandSetup:
		fmt.Fprintf(os.Stderr, "gohere %s is not implemented yet\n", cmd.Kind)
		os.Exit(1)
	default:
		fmt.Fprintln(os.Stderr, "unknown command")
		os.Exit(2)
	}
}
