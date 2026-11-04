package cli

import "testing"

func TestParsePortModes(t *testing.T) {
	for _, tt := range []struct {
		args    []string
		port    int
		connect bool
	}{
		{nil, 0, false},
		{[]string{":5173"}, 5173, false},
		{[]string{"+5173"}, 5173, true},
		{[]string{"--as", "api", "+1", "--open"}, 1, true},
		{[]string{":65535", "dev:web"}, 65535, false},
		{[]string{"dev:web", ":5173"}, 5173, false},
		{[]string{":5173", "--", "node", "server.js"}, 5173, false},
	} {
		t.Run(testArgsName(tt.args), func(t *testing.T) {
			cmd, err := Parse(append([]string{"gohere"}, tt.args...))
			if err != nil {
				t.Fatal(err)
			}
			if cmd.TargetPort != tt.port || cmd.Connect != tt.connect {
				t.Fatalf("command = %#v", cmd)
			}
			if len(tt.args) == 1 && (cmd.Script != "dev" || cmd.ExplicitScript || len(cmd.Scripts) != 0) {
				t.Fatalf("port should keep the default script: %#v", cmd)
			}
		})
	}
}

func testArgsName(args []string) string {
	if len(args) == 0 {
		return "default"
	}
	return args[0]
}

func TestParseRejectsInvalidPortModes(t *testing.T) {
	for _, args := range [][]string{
		{":"}, {"+"}, {":0"}, {"+0"}, {":65536"}, {"+65536"},
		{":abc"}, {"+abc"}, {":-1"}, {"++1"}, {":+1"},
		{":5173", "+5174"}, {"+5173", ":5174"},
		{":5173", ":5174"}, {"+5173", "+5174"},
		{"+5173", "dev"}, {"dev", "+5173"}, {"+5173", "list"}, {"+5173", "stop"},
		{"+5173", "./dist"}, {"./dist", "+5173"},
		{"+5173", "--live"}, {"+5173", "--port-flag", "--port"},
		{"+5173", "--", "node", "server.js"},
		{":5173", "dev:web", "dev:api"},
	} {
		t.Run(testArgsName(args), func(t *testing.T) {
			if cmd, err := Parse(append([]string{"gohere"}, args...)); err == nil {
				t.Fatalf("accepted %q: %#v", args, cmd)
			}
		})
	}
}
