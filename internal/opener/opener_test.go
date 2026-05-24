package opener

import (
	"context"
	"testing"
)

func TestCommandFor(t *testing.T) {
	tests := []struct {
		name string
		goos string
		wsl  bool
		url  string
		want []string
	}{
		{
			name: "linux",
			goos: "linux",
			url:  "http://project.localhost",
			want: []string{"xdg-open", "http://project.localhost"},
		},
		{
			name: "wsl",
			goos: "linux",
			wsl:  true,
			url:  "http://project.localhost",
			want: []string{"cmd.exe", "/c", "start", "", "http://project.localhost"},
		},
		{
			name: "darwin",
			goos: "darwin",
			url:  "http://project.localhost",
			want: []string{"open", "http://project.localhost"},
		},
		{
			name: "windows",
			goos: "windows",
			url:  "http://project.localhost",
			want: []string{"rundll32", "url.dll,FileProtocolHandler", "http://project.localhost"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CommandFor(tt.goos, tt.wsl, tt.url)
			if !sameStrings(got, tt.want) {
				t.Fatalf("CommandFor() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestOpenStartsCommandWithNonCanceledContext(t *testing.T) {
	oldStartCommand := startCommand
	defer func() {
		startCommand = oldStartCommand
	}()
	called := false
	startCommand = func(ctx context.Context, command string, args ...string) error {
		called = true
		if err := ctx.Err(); err != nil {
			t.Fatalf("open command context is canceled: %v", err)
		}
		if command != "xdg-open" {
			t.Fatalf("command = %q", command)
		}
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Open(ctx, "linux", false, "http://project.localhost"); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("start command was not called")
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
