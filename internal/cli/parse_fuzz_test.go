package cli

import (
	"strings"
	"testing"
)

func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		"",
		"list --json",
		"list --verbose",
		"stop --all",
		"dev:web dev:api --open",
		"--target 5173 -- npm run dev",
		"./dist --live",
		"service stop",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, input string) {
		args := append([]string{"gohere"}, strings.Fields(input)...)
		cmd, err := Parse(args)
		if err != nil {
			if !strings.HasPrefix(err.Error(), "gohere error:") &&
				!strings.HasPrefix(err.Error(), "Invalid alias:") {
				t.Fatalf("Parse(%#v) returned unprefixed error: %v", args, err)
			}
			return
		}
		if cmd.Kind == "" {
			t.Fatalf("Parse(%#v) returned empty command kind", args)
		}
		if cmd.TargetPath != "" && len(cmd.Scripts) > 0 {
			t.Fatalf("Parse(%#v) combined path target and scripts: %#v", args, cmd)
		}
		if cmd.Kind == CommandRaw && len(cmd.Raw) == 0 {
			t.Fatalf("Parse(%#v) returned raw command without args: %#v", args, cmd)
		}
		if cmd.JSON && cmd.Kind != CommandList {
			t.Fatalf("Parse(%#v) set JSON on non-list command: %#v", args, cmd)
		}
	})
}
