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
	Kind           CommandKind
	Script         string
	Scripts        []string
	TargetPath     string
	ExplicitScript bool
	Raw            []string
	Verbose        bool
	JSON           bool
	Open           bool
	Live           bool
	HTTP           bool
	As             string
	TargetPort     int
	PortFlag       string
	URLScheme      string
	ShareMode      string
	HelpTopic      string
	StopTarget     string
	StopAll        bool
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

		if cmd.TargetPath != "" && arg != "--" && !strings.HasPrefix(arg, "-") {
			return Command{}, pathTargetCombinationError()
		}

		switch arg {
		case "--":
			if cmd.TargetPath != "" {
				return Command{}, pathTargetCombinationError()
			}
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
		case "--live":
			cmd.Live = true
		case "--http":
			cmd.HTTP = true
		case "--share":
			return Command{}, parseError("--share requires a mode")
		case "--as":
			if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
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
			if len(rest) == 0 || isReservedValueFlag(rest[0]) {
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
				if cmd.Script == "" {
					cmd.Script = arg
				}
				cmd.Scripts = append(cmd.Scripts, arg)
				continue
			}
			return fixedCommand(CommandList, arg, rest)
		case "stop":
			if sawScript {
				cmd.Kind = CommandRun
				if cmd.Script == "" {
					cmd.Script = arg
				}
				cmd.Scripts = append(cmd.Scripts, arg)
				continue
			}
			return parseStop(rest)
		case "prune":
			if sawScript {
				cmd.Kind = CommandRun
				if cmd.Script == "" {
					cmd.Script = arg
				}
				cmd.Scripts = append(cmd.Scripts, arg)
				continue
			}
			return fixedCommand(CommandPrune, arg, rest)
		case "doctor":
			if sawScript {
				cmd.Kind = CommandRun
				if cmd.Script == "" {
					cmd.Script = arg
				}
				cmd.Scripts = append(cmd.Scripts, arg)
				continue
			}
			return fixedCommand(CommandDoctor, arg, rest)
		case "service":
			if sawScript {
				cmd.Kind = CommandRun
				if cmd.Script == "" {
					cmd.Script = arg
				}
				cmd.Scripts = append(cmd.Scripts, arg)
				continue
			}
			return parseService(rest)
		case "setup":
			if sawScript {
				cmd.Kind = CommandRun
				if cmd.Script == "" {
					cmd.Script = arg
				}
				cmd.Scripts = append(cmd.Scripts, arg)
				continue
			}
			return fixedCommand(CommandSetup, arg, rest)
		case "uninstall":
			if sawScript {
				cmd.Kind = CommandRun
				if cmd.Script == "" {
					cmd.Script = arg
				}
				cmd.Scripts = append(cmd.Scripts, arg)
				continue
			}
			return fixedCommand(CommandUninstall, arg, rest)
		default:
			if strings.HasPrefix(arg, "--share=") {
				mode := strings.TrimPrefix(arg, "--share=")
				if mode == "" {
					return Command{}, parseError("--share requires a mode")
				}
				if mode != "lan" {
					return Command{}, parseError(fmt.Sprintf("unsupported share mode %q. Available: lan", mode))
				}
				cmd.ShareMode = mode
				continue
			}
			if strings.HasPrefix(arg, "-") {
				return Command{}, unknownOptionError(arg)
			}
			if isExplicitPathArg(arg) {
				if sawScript || len(cmd.Scripts) > 0 || cmd.TargetPath != "" {
					return Command{}, pathTargetCombinationError()
				}
				cmd.Kind = CommandRun
				cmd.Script = ""
				cmd.TargetPath = arg
				continue
			}
			cmd.Kind = CommandRun
			cmd.ExplicitScript = true
			if cmd.Script == "dev" && len(cmd.Scripts) == 0 {
				cmd.Script = arg
			}
			cmd.Scripts = append(cmd.Scripts, arg)
			sawScript = true
		}
	}

	if err := validateMultiRun(cmd); err != nil {
		return Command{}, err
	}
	return cmd, nil
}

func validateMultiRun(cmd Command) error {
	if cmd.TargetPath != "" && len(cmd.Scripts) > 0 {
		return pathTargetCombinationError()
	}
	if cmd.Kind != CommandRun || len(cmd.Scripts) <= 1 {
		return nil
	}
	if cmd.As != "" {
		return parseError("--as can only be used with one project")
	}
	if cmd.TargetPort != 0 {
		return parseError("--target can only be used with one project")
	}
	if cmd.ShareMode != "" {
		return parseError("--share=lan can only be used with one project")
	}
	return nil
}

func isExplicitPathArg(arg string) bool {
	if arg == "." || arg == ".." {
		return true
	}
	return strings.HasPrefix(arg, "./") ||
		strings.HasPrefix(arg, "../") ||
		strings.HasPrefix(arg, `/`) ||
		strings.HasPrefix(arg, `.\\`) ||
		strings.HasPrefix(arg, `..\\`) ||
		isWindowsDrivePath(arg)
}

func isWindowsDrivePath(arg string) bool {
	if len(arg) < 3 {
		return false
	}
	drive := arg[0]
	if !((drive >= 'A' && drive <= 'Z') || (drive >= 'a' && drive <= 'z')) {
		return false
	}
	return arg[1] == ':' && (arg[2] == '\\' || arg[2] == '/')
}

func pathTargetCombinationError() error {
	return parseError("Path targets cannot be combined with scripts yet.")
}

func parseStop(args []string) (Command, error) {
	cmd := Command{Kind: CommandStop}
	for _, arg := range args {
		switch arg {
		case "--help", "-h":
			return helpCommand("stop"), nil
		case "--all":
			if cmd.StopTarget != "" {
				return Command{}, parseError("stop accepts either --all or one route/project")
			}
			cmd.StopAll = true
		default:
			if strings.HasPrefix(arg, "-") {
				return Command{}, unknownOptionError(arg)
			}
			if cmd.StopAll || cmd.StopTarget != "" {
				return Command{}, parseError("stop accepts either --all or one route/project")
			}
			cmd.StopTarget = arg
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
		case "--json":
			if kind != CommandList {
				return Command{}, errors.New("gohere error: --json is only supported for list")
			}
			cmd.JSON = true
		case "--open", "-o":
			return Command{}, openAfterFixedCommandError()
		case "--http":
			return Command{}, httpAfterFixedCommandError()
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

func isReservedValueFlag(arg string) bool {
	switch arg {
	case "--verbose", "--open", "-o", "--live", "--http", "--as", "--target", "--port-flag", "--help", "-h", "--version", "-v":
		return true
	default:
		return false
	}
}

func openAfterFixedCommandError() error {
	return errors.New("gohere error: --open is only supported when running a project")
}

func httpAfterFixedCommandError() error {
	return errors.New("gohere error: --http is only supported when running a project")
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
