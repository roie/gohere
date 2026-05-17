package cli

import (
	"errors"
	"strconv"
)

type CommandKind string

const (
	CommandRun    CommandKind = "run"
	CommandRaw    CommandKind = "raw"
	CommandList   CommandKind = "list"
	CommandStop   CommandKind = "stop"
	CommandClean  CommandKind = "clean"
	CommandDoctor CommandKind = "doctor"
	CommandRouter CommandKind = "router"
	CommandSetup  CommandKind = "setup"
)

type Command struct {
	Kind       CommandKind
	Script     string
	Raw        []string
	Verbose    bool
	TargetPort int
	PortFlag   string
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
				return Command{}, errors.New("raw command after -- is required")
			}
			cmd.Kind = CommandRaw
			cmd.Script = ""
			cmd.Raw = rest
			return cmd, nil
		case "--verbose":
			cmd.Verbose = true
		case "--target":
			if len(rest) == 0 {
				return Command{}, errors.New("--target requires a port")
			}
			port, err := strconv.Atoi(rest[0])
			if err != nil || port <= 0 || port > 65535 {
				return Command{}, errors.New("--target must be a valid port")
			}
			cmd.TargetPort = port
			rest = rest[1:]
		case "--port-flag":
			if len(rest) == 0 {
				return Command{}, errors.New("--port-flag requires a flag")
			}
			cmd.PortFlag = rest[0]
			rest = rest[1:]
		case "list":
			if sawScript {
				cmd.Kind = CommandRun
				cmd.Script = arg
				continue
			}
			cmd.Kind = CommandList
			cmd.Script = ""
			return cmd, nil
		case "stop":
			if sawScript {
				cmd.Kind = CommandRun
				cmd.Script = arg
				continue
			}
			cmd.Kind = CommandStop
			cmd.Script = ""
			return cmd, nil
		case "clean":
			if sawScript {
				cmd.Kind = CommandRun
				cmd.Script = arg
				continue
			}
			cmd.Kind = CommandClean
			cmd.Script = ""
			return cmd, nil
		case "doctor":
			if sawScript {
				cmd.Kind = CommandRun
				cmd.Script = arg
				continue
			}
			cmd.Kind = CommandDoctor
			cmd.Script = ""
			return cmd, nil
		case "router":
			if sawScript {
				cmd.Kind = CommandRun
				cmd.Script = arg
				continue
			}
			cmd.Kind = CommandRouter
			cmd.Script = ""
			return cmd, nil
		case "setup":
			if sawScript {
				cmd.Kind = CommandRun
				cmd.Script = arg
				continue
			}
			cmd.Kind = CommandSetup
			cmd.Script = ""
			return cmd, nil
		default:
			cmd.Kind = CommandRun
			cmd.Script = arg
			sawScript = true
		}
	}

	return cmd, nil
}
