package opener

import "testing"

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
