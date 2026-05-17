package main

import (
	"fmt"
	"os"

	"github.com/roie/gohere/internal/cli"
)

func main() {
	cmd, err := cli.Parse(os.Args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	switch cmd.Kind {
	case cli.CommandRun, cli.CommandRaw:
		fmt.Fprintln(os.Stderr, "gohere run is not implemented yet")
		os.Exit(1)
	case cli.CommandList, cli.CommandStop, cli.CommandClean, cli.CommandDoctor, cli.CommandRouter, cli.CommandSetup:
		fmt.Fprintf(os.Stderr, "gohere %s is not implemented yet\n", cmd.Kind)
		os.Exit(1)
	default:
		fmt.Fprintln(os.Stderr, "unknown command")
		os.Exit(2)
	}
}
