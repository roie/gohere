package runner

import (
	"reflect"
	"testing"
)

func TestInjectBrowserArgs(t *testing.T) {
	for _, command := range []string{
		"vite --open /docs", "vite preview", "svelte-kit dev", "astro dev", "vp dev",
		"env NODE_ENV=development ./node_modules/.bin/vite --host localhost --port 3000",
		"webpack serve --open", "webpack-cli serve", "webpack-dev-server", "rspack dev", "rspack serve", "rspack s", "rspack preview", "rsbuild", "rsbuild dev", "rsbuild preview",
	} {
		t.Run(command, func(t *testing.T) {
			for _, args := range [][]string{nil, {"--", "--port", "3000"}, {"vite", "--open"}} {
				want := append([]string(nil), args...)
				if len(want) == 0 {
					want = []string{"--"}
				}
				want = append(want, "--no-open")
				if got := InjectBrowserArgs(command, args); !reflect.DeepEqual(got, want) {
					t.Fatalf("args=%q: got %q, want %q", args, got, want)
				}
			}
		})
	}
	for _, command := range []string{"node server.js", "nuxt dev", "ng serve", "vue-cli-service serve", "parcel serve index.html", "next dev", "wrangler dev", "webpack build", "webpack-cli build", "rspack build", "rsbuild build"} {
		args := []string{"--", "--port", "3000"}
		if got := InjectBrowserArgs(command, args); !reflect.DeepEqual(got, args) {
			t.Fatalf("unsupported %q: got %q, want %q", command, got, args)
		}
	}
}
