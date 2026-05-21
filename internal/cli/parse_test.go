package cli

import "testing"

func TestParseDefaultRun(t *testing.T) {
	cmd, err := Parse([]string{"gohere"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Kind != CommandRun || cmd.Script != "dev" {
		t.Fatalf("Parse default = %#v", cmd)
	}
}

func TestParseScriptRun(t *testing.T) {
	cmd, err := Parse([]string{"gohere", "dev:web"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Kind != CommandRun || cmd.Script != "dev:web" {
		t.Fatalf("Parse script = %#v", cmd)
	}
}

func TestParseVersionScriptRun(t *testing.T) {
	cmd, err := Parse([]string{"gohere", "version"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Kind != CommandRun || cmd.Script != "version" {
		t.Fatalf("Parse version script = %#v", cmd)
	}
}

func TestParseVersionFlags(t *testing.T) {
	for _, arg := range []string{"--version", "-v"} {
		t.Run(arg, func(t *testing.T) {
			cmd, err := Parse([]string{"gohere", arg})
			if err != nil {
				t.Fatal(err)
			}
			if cmd.Kind != CommandVersion {
				t.Fatalf("Parse version flag = %#v", cmd)
			}
		})
	}
}

func TestParseFileTargetRun(t *testing.T) {
	cmd, err := Parse([]string{"gohere", "pages/about.html"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Kind != CommandRun || cmd.Script != "pages/about.html" {
		t.Fatalf("Parse file target = %#v", cmd)
	}
}

func TestParseRawCommand(t *testing.T) {
	cmd, err := Parse([]string{"gohere", "--", "npm", "run", "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Kind != CommandRaw {
		t.Fatalf("kind = %v", cmd.Kind)
	}
	want := []string{"npm", "run", "dev"}
	if len(cmd.Raw) != len(want) {
		t.Fatalf("raw = %#v", cmd.Raw)
	}
	for i := range want {
		if cmd.Raw[i] != want[i] {
			t.Fatalf("raw = %#v", cmd.Raw)
		}
	}
}

func TestParseRawCommandRequiresCommand(t *testing.T) {
	_, err := Parse([]string{"gohere", "--"})
	if err == nil {
		t.Fatal("expected error")
	}
	want := "gohere error: raw command after -- is required"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

func TestParseOpenFlag(t *testing.T) {
	tests := [][]string{
		{"gohere", "--open"},
		{"gohere", "-o"},
		{"gohere", "dev:web", "--open"},
		{"gohere", "dev:web", "-o"},
	}

	for _, args := range tests {
		t.Run(args[len(args)-1], func(t *testing.T) {
			cmd, err := Parse(args)
			if err != nil {
				t.Fatal(err)
			}
			if !cmd.Open {
				t.Fatalf("Open = false for args %#v", args)
			}
		})
	}
}

func TestParseAsFlag(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "default run", args: []string{"gohere", "--as", "api"}, want: "api"},
		{name: "script run", args: []string{"gohere", "dev:web", "--as", "web"}, want: "web"},
		{name: "file run", args: []string{"gohere", "about.html", "--as", "docs"}, want: "docs"},
		{name: "raw run", args: []string{"gohere", "--as", "api", "--", "npm", "run", "dev"}, want: "api"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, err := Parse(tt.args)
			if err != nil {
				t.Fatal(err)
			}
			if cmd.As != tt.want {
				t.Fatalf("As = %q, want %q for args %#v", cmd.As, tt.want, tt.args)
			}
		})
	}
}

func TestParseAsFlagRequiresValue(t *testing.T) {
	_, err := Parse([]string{"gohere", "--as"})
	if err == nil {
		t.Fatal("expected error")
	}
	want := "gohere error: --as requires a name"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

func TestParseRawCommandOpenFlag(t *testing.T) {
	cmd, err := Parse([]string{"gohere", "--open", "--", "npm", "run", "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Kind != CommandRaw || !cmd.Open || !sameStrings(cmd.Raw, []string{"npm", "run", "dev"}) {
		t.Fatalf("Parse raw open = %#v", cmd)
	}
}

func TestParseFixedCommands(t *testing.T) {
	tests := map[string]CommandKind{
		"list":      CommandList,
		"stop":      CommandStop,
		"prune":     CommandPrune,
		"doctor":    CommandDoctor,
		"setup":     CommandSetup,
		"uninstall": CommandUninstall,
	}

	for arg, want := range tests {
		t.Run(arg, func(t *testing.T) {
			cmd, err := Parse([]string{"gohere", arg})
			if err != nil {
				t.Fatal(err)
			}
			if cmd.Kind != want {
				t.Fatalf("kind = %v, want %v", cmd.Kind, want)
			}
		})
	}
}

func TestParseRouterIsScriptName(t *testing.T) {
	cmd, err := Parse([]string{"gohere", "router"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Kind != CommandRun || cmd.Script != "router" {
		t.Fatalf("Parse router = %#v", cmd)
	}
}

func TestParseServiceCommands(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want CommandKind
	}{
		{name: "stop", args: []string{"gohere", "service", "stop"}, want: CommandServiceStop},
		{name: "run", args: []string{"gohere", "service", "run"}, want: CommandServiceRun},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, err := Parse(tt.args)
			if err != nil {
				t.Fatal(err)
			}
			if cmd.Kind != tt.want || cmd.Script != "" {
				t.Fatalf("Parse service command = %#v, want %v", cmd, tt.want)
			}
		})
	}
}

func TestParseServiceRejectsUnknownSubcommand(t *testing.T) {
	_, err := Parse([]string{"gohere", "service", "status"})
	if err == nil {
		t.Fatal("expected error")
	}
	want := "gohere error: unknown service command \"status\"\nTry:\n  gohere service stop"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

func TestCleanIsParsedAsScript(t *testing.T) {
	cmd, err := Parse([]string{"gohere", "clean"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Kind != CommandRun || cmd.Script != "clean" {
		t.Fatalf("Parse clean = %#v", cmd)
	}
}

func TestParseHelp(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantTopic string
	}{
		{name: "global long", args: []string{"gohere", "--help"}},
		{name: "global short", args: []string{"gohere", "-h"}},
		{name: "global command", args: []string{"gohere", "help"}},
		{name: "setup topic", args: []string{"gohere", "setup", "--help"}, wantTopic: "setup"},
		{name: "doctor topic short", args: []string{"gohere", "doctor", "-h"}, wantTopic: "doctor"},
		{name: "service topic", args: []string{"gohere", "service", "--help"}, wantTopic: "service"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, err := Parse(tt.args)
			if err != nil {
				t.Fatal(err)
			}
			if cmd.Kind != CommandHelp || cmd.HelpTopic != tt.wantTopic {
				t.Fatalf("Parse help = %#v", cmd)
			}
		})
	}
}

func TestParseVerboseAfterFixedCommand(t *testing.T) {
	cmd, err := Parse([]string{"gohere", "list", "--verbose"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Kind != CommandList || !cmd.Verbose {
		t.Fatalf("Parse list --verbose = %#v", cmd)
	}
}

func TestParseHelpAfterFixedCommandWithOtherFlags(t *testing.T) {
	cmd, err := Parse([]string{"gohere", "doctor", "--verbose", "--help"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Kind != CommandHelp || cmd.HelpTopic != "doctor" {
		t.Fatalf("Parse doctor --verbose --help = %#v", cmd)
	}
}

func TestParseRejectsOpenAfterFixedCommand(t *testing.T) {
	_, err := Parse([]string{"gohere", "list", "--open"})
	if err == nil {
		t.Fatal("expected error")
	}
	want := "gohere error: --open is only supported when running a project"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

func TestParseRejectsOpenAfterFixedCommandWithOtherFlags(t *testing.T) {
	_, err := Parse([]string{"gohere", "list", "--verbose", "--open"})
	if err == nil {
		t.Fatal("expected error")
	}
	want := "gohere error: --open is only supported when running a project"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

func TestParseOptions(t *testing.T) {
	cmd, err := Parse([]string{"gohere", "--verbose", "--target", "5173", "--port-flag", "-p", "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if !cmd.Verbose || cmd.TargetPort != 5173 || cmd.PortFlag != "-p" || cmd.Script != "dev" {
		t.Fatalf("options = %#v", cmd)
	}
}

func TestParseOptionErrorsUseGoherePrefix(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing target", args: []string{"gohere", "--target"}, want: "gohere error: --target requires a port"},
		{name: "invalid target", args: []string{"gohere", "--target", "nope"}, want: "gohere error: --target must be a valid port"},
		{name: "missing port flag", args: []string{"gohere", "--port-flag"}, want: "gohere error: --port-flag requires a flag"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.args)
			if err == nil {
				t.Fatal("expected error")
			}
			if err.Error() != tt.want {
				t.Fatalf("error = %q, want %q", err.Error(), tt.want)
			}
		})
	}
}

func TestParseOptionsAfterScript(t *testing.T) {
	cmd, err := Parse([]string{"gohere", "dev:web", "--target", "5173", "--port-flag", "--listen", "--verbose"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Kind != CommandRun || cmd.Script != "dev:web" || cmd.TargetPort != 5173 || cmd.PortFlag != "--listen" || !cmd.Verbose {
		t.Fatalf("options after script = %#v", cmd)
	}
}

func TestParseRejectsUnknownFlag(t *testing.T) {
	_, err := Parse([]string{"gohere", "--list"})
	if err == nil {
		t.Fatal("expected error")
	}
	want := "gohere error: unknown option \"--list\"\nTry:\n  gohere list"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

func TestParseRawCommandPreservesTrailingFlags(t *testing.T) {
	cmd, err := Parse([]string{"gohere", "--target", "5173", "--", "npm", "run", "dev", "--", "--host", "0.0.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"npm", "run", "dev", "--", "--host", "0.0.0.0"}
	if !sameStrings(cmd.Raw, want) || cmd.TargetPort != 5173 {
		t.Fatalf("raw command = %#v", cmd)
	}
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
