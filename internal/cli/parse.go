package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type CommandKind string

const (
	CommandRun         CommandKind = "run"
	CommandRaw         CommandKind = "raw"
	CommandList        CommandKind = "list"
	CommandStop        CommandKind = "stop"
	CommandPrune       CommandKind = "prune"
	CommandDoctor      CommandKind = "doctor"
	CommandServiceRun  CommandKind = "service-run"
	CommandServiceStop CommandKind = "service-stop"
	CommandSetup       CommandKind = "setup"
	CommandUninstall   CommandKind = "uninstall"
	CommandHelp        CommandKind = "help"
	CommandVersion     CommandKind = "version"
)

type Command struct {
	Kind       CommandKind
	Script     string
	Raw        []string
	Verbose    bool
	Open       bool
	As         string
	TargetPort int
	PortFlag   string
	HelpTopic  string
}

func Parse(args []string) (Command, error) {
	cmd := Command{Kind: CommandRun, Script: "dev"}
	if len(args) <= 1 {
		return cmd, nil
	}

	sawScript := false
	rest := append([]string(nil), args[1:]...)
	for len(rest) > 0 {
		arg := rest[0]
		rest = rest[1:]

		switch arg {
		case "--":
			if len(rest) == 0 {
				return Command{}, parseError("raw command after -- is required")
			}
			cmd.Kind = CommandRaw
			cmd.Script = ""
			cmd.Raw = rest
			return cmd, nil
		case "--verbose":
			cmd.Verbose = true
		case "--open", "-o":
			cmd.Open = true
		case "--as":
			if len(rest) == 0 {
				return Command{}, parseError("--as requires a name")
			}
			cmd.As = rest[0]
			rest = rest[1:]
		case "--target":
			if len(rest) == 0 {
				return Command{}, parseError("--target requires a port")
			}
			port, err := strconv.Atoi(rest[0])
			if err != nil || port <= 0 || port > 65535 {
				return Command{}, parseError("--target must be a valid port")
			}
			cmd.TargetPort = port
			rest = rest[1:]
		case "--port-flag":
			if len(rest) == 0 {
				return Command{}, parseError("--port-flag requires a flag")
			}
			cmd.PortFlag = rest[0]
			rest = rest[1:]
		case "--help", "-h", "help":
			cmd.Kind = CommandHelp
			cmd.Script = ""
			return cmd, nil
		case "--version", "-v":
			cmd.Kind = CommandVersion
			cmd.Script = ""
			return cmd, nil
		case "list":
			if sawScript {
				cmd.Kind = CommandRun
				cmd.Script = arg
				continue
			}
			return fixedCommand(CommandList, arg, rest)
		case "stop":
			if sawScript {
				cmd.Kind = CommandRun
				cmd.Script = arg
				continue
			}
			return fixedCommand(CommandStop, arg, rest)
		case "prune":
			if sawScript {
				cmd.Kind = CommandRun
				cmd.Script = arg
				continue
			}
			return fixedCommand(CommandPrune, arg, rest)
		case "doctor":
			if sawScript {
				cmd.Kind = CommandRun
				cmd.Script = arg
				continue
			}
			return fixedCommand(CommandDoctor, arg, rest)
		case "service":
			if sawScript {
				cmd.Kind = CommandRun
				cmd.Script = arg
				continue
			}
			return parseService(rest)
		case "setup":
			if sawScript {
				cmd.Kind = CommandRun
				cmd.Script = arg
				continue
			}
			return fixedCommand(CommandSetup, arg, rest)
		case "uninstall":
			if sawScript {
				cmd.Kind = CommandRun
				cmd.Script = arg
				continue
			}
			return fixedCommand(CommandUninstall, arg, rest)
		default:
			if strings.HasPrefix(arg, "-") {
				return Command{}, unknownOptionError(arg)
			}
			cmd.Kind = CommandRun
			cmd.Script = arg
			sawScript = true
		}
	}

	return cmd, nil
}

func parseService(args []string) (Command, error) {
	if len(args) == 0 {
		return Command{}, errors.New("gohere error: service requires a command\nTry:\n  gohere service stop")
	}
	if helpRequested(args) {
		return helpCommand("service"), nil
	}
	subcommand := args[0]
	rest := args[1:]
	if helpRequested(rest) {
		return helpCommand("service " + subcommand), nil
	}
	if verboseRequested(rest) || openRequested(rest) || len(rest) > 0 {
		return Command{}, fmt.Errorf("gohere error: unknown service command %q\nTry:\n  gohere service stop", subcommand)
	}
	switch subcommand {
	case "stop":
		return Command{Kind: CommandServiceStop}, nil
	case "run":
		return Command{Kind: CommandServiceRun}, nil
	default:
		return Command{}, fmt.Errorf("gohere error: unknown service command %q\nTry:\n  gohere service stop", subcommand)
	}
}

func fixedCommand(kind CommandKind, topic string, args []string) (Command, error) {
	cmd := Command{Kind: kind}
	for _, arg := range args {
		switch arg {
		case "--help", "-h":
			return helpCommand(topic), nil
		case "--verbose":
			cmd.Verbose = true
		case "--open", "-o":
			return Command{}, openAfterFixedCommandError()
		default:
			if strings.HasPrefix(arg, "-") {
				return Command{}, unknownOptionError(arg)
			}
			return Command{}, fmt.Errorf("gohere error: unexpected argument %q", arg)
		}
	}
	return cmd, nil
}

func helpRequested(args []string) bool {
	if len(args) != 1 {
		return false
	}
	return args[0] == "--help" || args[0] == "-h"
}

func helpCommand(topic string) Command {
	return Command{Kind: CommandHelp, HelpTopic: topic}
}

func verboseRequested(args []string) bool {
	return len(args) == 1 && args[0] == "--verbose"
}

func openRequested(args []string) bool {
	return len(args) == 1 && (args[0] == "--open" || args[0] == "-o")
}

func openAfterFixedCommandError() error {
	return errors.New("gohere error: --open is only supported when running a project")
}

func parseError(message string) error {
	return fmt.Errorf("gohere error: %s", message)
}

func unknownOptionError(arg string) error {
	suggestions := map[string]string{
		"--doctor":    "doctor",
		"--list":      "list",
		"--prune":     "prune",
		"--setup":     "setup",
		"--stop":      "stop",
		"--uninstall": "uninstall",
	}
	if command, ok := suggestions[arg]; ok {
		return fmt.Errorf("gohere error: unknown option %q\nTry:\n  gohere %s", arg, command)
	}
	return fmt.Errorf("gohere error: unknown option %q", arg)
}
