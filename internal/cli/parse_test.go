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

func TestParseFixedCommands(t *testing.T) {
	tests := map[string]CommandKind{
		"list":   CommandList,
		"stop":   CommandStop,
		"clean":  CommandClean,
		"doctor": CommandDoctor,
		"router": CommandRouter,
		"setup":  CommandSetup,
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

func TestParseOptions(t *testing.T) {
	cmd, err := Parse([]string{"gohere", "--verbose", "--target", "5173", "--port-flag", "-p", "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if !cmd.Verbose || cmd.TargetPort != 5173 || cmd.PortFlag != "-p" || cmd.Script != "dev" {
		t.Fatalf("options = %#v", cmd)
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
