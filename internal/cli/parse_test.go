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
	if cmd.ExplicitScript {
		t.Fatalf("bare gohere should not mark dev as explicit: %#v", cmd)
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
	if !cmd.ExplicitScript {
		t.Fatalf("script argument should be explicit: %#v", cmd)
	}
}

func TestParseExplicitDefaultScriptRun(t *testing.T) {
	cmd, err := Parse([]string{"gohere", "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Kind != CommandRun || cmd.Script != "dev" || !cmd.ExplicitScript {
		t.Fatalf("Parse explicit dev = %#v", cmd)
	}
}

func TestParseExplicitNonDefaultScriptRun(t *testing.T) {
	cmd, err := Parse([]string{"gohere", "build"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Kind != CommandRun || cmd.Script != "build" || !cmd.ExplicitScript {
		t.Fatalf("Parse explicit build = %#v", cmd)
	}
}

func TestParseMultiScriptRun(t *testing.T) {
	cmd, err := Parse([]string{"gohere", "dev:web", "dev:api", "--open", "--verbose"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Kind != CommandRun || cmd.Script != "dev:web" || !sameStrings(cmd.Scripts, []string{"dev:web", "dev:api"}) || !cmd.Open || !cmd.Verbose {
		t.Fatalf("Parse multi script = %#v", cmd)
	}
}

func TestParseMultiRejectsUnsupportedOptions(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "as", args: []string{"gohere", "dev:web", "dev:api", "--as", "web"}, want: "gohere error: --as can only be used with one project"},
		{name: "target", args: []string{"gohere", "dev:web", "dev:api", "--target", "5173"}, want: "gohere error: --target can only be used with one project"},
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

func TestParseValueFlagsRejectOptionAsValue(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "as", args: []string{"gohere", "--as", "--target", "3000"}, want: "gohere error: --as requires a name"},
		{name: "port flag", args: []string{"gohere", "--port-flag", "--target", "3000"}, want: "gohere error: --port-flag requires a flag"},
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

func TestParseExplicitPathTarget(t *testing.T) {
	tests := []string{".", "..", "./dist", "../site", "/tmp/site", `.\\dist`, `..\\site`, `C:\\site`}
	for _, arg := range tests {
		t.Run(arg, func(t *testing.T) {
			cmd, err := Parse([]string{"gohere", arg})
			if err != nil {
				t.Fatal(err)
			}
			if cmd.Kind != CommandRun || cmd.TargetPath != arg || cmd.Script != "" {
				t.Fatalf("Parse path target = %#v, want TargetPath %q", cmd, arg)
			}
		})
	}
}

func TestParseNonExplicitPathsRemainScripts(t *testing.T) {
	tests := []string{"dist", "dist/", "apps/web", "dev:web"}
	for _, arg := range tests {
		t.Run(arg, func(t *testing.T) {
			cmd, err := Parse([]string{"gohere", arg})
			if err != nil {
				t.Fatal(err)
			}
			if cmd.Kind != CommandRun || cmd.Script != arg || cmd.TargetPath != "" {
				t.Fatalf("Parse script = %#v, want script %q", cmd, arg)
			}
		})
	}
}

func TestParsePathTargetRejectsExtraScripts(t *testing.T) {
	_, err := Parse([]string{"gohere", "./apps/web", "dev:api"})
	if err == nil {
		t.Fatal("expected error")
	}
	want := "gohere error: Path targets cannot be combined with scripts yet."
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
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

func TestParseLiveFlag(t *testing.T) {
	tests := [][]string{
		{"gohere", "--live"},
		{"gohere", "./dist", "--live"},
		{"gohere", "about.html", "--live"},
		{"gohere", "--live", "--", "npm", "run", "dev"},
	}

	for _, args := range tests {
		t.Run(args[len(args)-1], func(t *testing.T) {
			cmd, err := Parse(args)
			if err != nil {
				t.Fatal(err)
			}
			if !cmd.Live {
				t.Fatalf("Live = false for args %#v", args)
			}
		})
	}
}

func TestParseHTTPFlag(t *testing.T) {
	tests := [][]string{
		{"gohere", "--http"},
		{"gohere", "dev:web", "--http"},
		{"gohere", "./dist", "--http"},
		{"gohere", "--http", "--", "npm", "run", "dev"},
	}

	for _, args := range tests {
		t.Run(args[len(args)-1], func(t *testing.T) {
			cmd, err := Parse(args)
			if err != nil {
				t.Fatal(err)
			}
			if !cmd.HTTP {
				t.Fatalf("HTTP = false for args %#v", args)
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

func TestParseStopTarget(t *testing.T) {
	cmd, err := Parse([]string{"gohere", "stop", "ctrltube"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Kind != CommandStop {
		t.Fatalf("kind = %v, want %v", cmd.Kind, CommandStop)
	}
	if cmd.StopTarget != "ctrltube" {
		t.Fatalf("StopTarget = %q, want ctrltube", cmd.StopTarget)
	}
}

func TestParseStopAll(t *testing.T) {
	cmd, err := Parse([]string{"gohere", "stop", "--all"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Kind != CommandStop {
		t.Fatalf("kind = %v, want %v", cmd.Kind, CommandStop)
	}
	if !cmd.StopAll {
		t.Fatal("StopAll = false, want true")
	}
}

func TestParseStopRejectsAllWithTarget(t *testing.T) {
	_, err := Parse([]string{"gohere", "stop", "--all", "web"})
	if err == nil {
		t.Fatal("expected error")
	}
	want := "gohere error: stop accepts either --all or one route/project"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
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

func TestParseListJSON(t *testing.T) {
	cmd, err := Parse([]string{"gohere", "list", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Kind != CommandList || !cmd.JSON {
		t.Fatalf("Parse list --json = %#v", cmd)
	}
}

func TestParseRejectsJSONAfterNonListFixedCommand(t *testing.T) {
	_, err := Parse([]string{"gohere", "doctor", "--json"})
	if err == nil {
		t.Fatal("expected error")
	}
	want := "gohere error: --json is only supported for list"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
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

func TestParseRejectsHTTPAfterFixedCommand(t *testing.T) {
	_, err := Parse([]string{"gohere", "list", "--http"})
	if err == nil {
		t.Fatal("expected error")
	}
	want := "gohere error: --http is only supported when running a project"
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

func TestParseShareLAN(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		kind       CommandKind
		wantScript string
		wantRaw    []string
	}{
		{name: "default", args: []string{"gohere", "--share=lan"}, kind: CommandRun, wantScript: "dev"},
		{name: "script", args: []string{"gohere", "dev:web", "--share=lan"}, kind: CommandRun, wantScript: "dev:web"},
		{name: "raw command", args: []string{"gohere", "--share=lan", "--", "npm", "run", "dev"}, kind: CommandRaw, wantRaw: []string{"npm", "run", "dev"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, err := Parse(tt.args)
			if err != nil {
				t.Fatal(err)
			}
			if cmd.Kind != tt.kind || cmd.Script != tt.wantScript || cmd.ShareMode != "lan" || !sameStrings(cmd.Raw, tt.wantRaw) {
				t.Fatalf("command = %#v", cmd)
			}
		})
	}
}

func TestParseRejectsLANShareForMultipleScripts(t *testing.T) {
	_, err := Parse([]string{"gohere", "dev:web", "dev:api", "--share=lan"})
	if err == nil || err.Error() != "gohere error: --share=lan can only be used with one project" {
		t.Fatalf("error = %v", err)
	}
}

func TestParseRejectsInvalidShareMode(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{args: []string{"gohere", "--share"}, want: "gohere error: --share requires a mode"},
		{args: []string{"gohere", "--share="}, want: "gohere error: --share requires a mode"},
		{args: []string{"gohere", "--share=public"}, want: "gohere error: unsupported share mode \"public\". Available: lan"},
	}
	for _, tt := range tests {
		_, err := Parse(tt.args)
		if err == nil || err.Error() != tt.want {
			t.Fatalf("Parse(%q) error = %v, want %q", tt.args, err, tt.want)
		}
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
