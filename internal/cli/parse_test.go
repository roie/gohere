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

func TestParseOptions(t *testing.T) {
	cmd, err := Parse([]string{"gohere", "--verbose", "--target", "5173", "--port-flag", "-p", "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if !cmd.Verbose || cmd.TargetPort != 5173 || cmd.PortFlag != "-p" || cmd.Script != "dev" {
		t.Fatalf("options = %#v", cmd)
	}
}
