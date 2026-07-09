# gohere

Tiny local dev URL launcher for `.localhost` projects.

```text
https://myproject.localhost
```

Run `gohere` inside a package project, workspace root, or static folder. It starts or serves the project on a hidden local port, routes a clean `.localhost` hostname to it, and prints the URL.

No script edits. No port memorization. No repo config.

## Install

Global install is recommended:

```bash
npm i -g gohere
```

Or install the Go CLI directly:

```bash
go install github.com/roie/gohere/cmd/gohere@latest
```

## Quick start

`gohere` supports package projects, workspace roots, and static files.

Run the default command:

```bash
gohere
```

In a package project, this runs the nearest `package.json` `dev` script. In a workspace root with child packages that have `dev` scripts, this starts each matching package and gives each package its own route:

```text
gohere web    -> https://web.myrepo.localhost
gohere worker -> https://worker.myrepo.localhost
```

If workspace metadata exists but no child package has a `dev` script, `gohere` falls back to the current package's `dev` script. If there is no package script and the folder has `index.html`, `gohere` serves it as a static site.

Run a named package script:

```bash
gohere dev:web
```

Run multiple server scripts:

```bash
gohere dev:web dev:api
```

When one `gohere` run starts multiple services, each service can discover the others through env vars. For example, a web dev server can proxy API requests to a worker without hardcoding a port:

```ts
target: process.env.GOHERE_WORKER_URL
```

Use `GOHERE_<NAME>_URL` for app config. `GOHERE_<NAME>_PORT`, `GOHERE_<NAME>_TARGET`, and `GOHERE_SERVICES_JSON` are also available when multiple managed services start together. `PORT` and `TARGET` are advanced values and are only set when `gohere` controls that service port.

Run any current package script exactly as written by naming it explicitly:

```bash
gohere dev
gohere build
gohere preview
```

Run an explicit filesystem target:

```bash
gohere ./dist
gohere ./apps/web
```

Auto-refresh static pages while editing:

```bash
gohere --live
```

Run a raw command:

```bash
gohere -- npm run dev
```

Use a custom `.localhost` name for this run:

```bash
gohere --as api
```

Route to a known target port:

```bash
gohere --target 5173 -- npm run dev
```

Use a custom port flag for tools that do not use `--port`:

```bash
gohere --port-flag --local-port dev
```

Open the project URL in your browser:

```bash
gohere --open
```

For static folders, `gohere` serves `index.html`. You can also open a specific file, for example `gohere about.html`, which routes to `https://myproject.localhost/about.html`.

CSS, images, and scripts are served normally.

## Examples

```text
myproject      -> https://myproject.localhost
@scope/web     -> https://web.localhost
./apps/web     -> https://web.repo.localhost
./dist         -> https://dist.localhost
about.html     -> https://myproject.localhost/about.html
```

## Route management

```bash
gohere list
gohere list --verbose
gohere list --json
gohere stop
gohere stop web
gohere stop --all
gohere prune
gohere doctor
gohere service stop
gohere uninstall
```

`gohere list --verbose` shows host, target, status, PID, and working directory.

`gohere list --json` returns the same route information in a stable machine-readable format.

`gohere stop` stops routes for the current folder. `gohere stop <target>` stops a listed route by host, short host label, route name, or project name. `gohere stop --all` stops safely controllable routes and skips unverified live routes.

Route status can be `ready`, `dead`, or `unknown`. `prune` removes only routes that are confidently dead.

## Service And Uninstall

Stop the background service without removing gohere:

```bash
gohere service stop
```

Clean up the copied service binary before removing the npm package:

```bash
gohere uninstall
npm uninstall -g gohere
```

`gohere uninstall` removes the local service install and asks before deleting routes, logs, and token state.

## How it works

`gohere` runs one local service on HTTP port `80` and HTTPS port `443`.

Each project gets a hidden local port. The service maps the clean `.localhost` hostname to that port using the request `Host` header.

First-time setup installs the local service in `~/.gohere/`, installs a local trusted certificate authority, and starts the service in the background. After that, `gohere` only starts your project and registers its route.

The service only serves local machine traffic. On macOS, gohere uses a port `80` listener that rejects non-loopback connections before requests reach the router.

State is stored in:

```text
~/.gohere/
```

Linux may ask for one-time permission to bind local ports and trust the local certificate authority.

When used from WSL, `gohere` reuses a running Windows service automatically.

## Platform support

- Linux / WSL
- Windows
- macOS (experimental)

The npm package includes x64 and arm64 binaries for these platforms.

## Limits

- `.localhost` only
- HTTPS uses a local trusted certificate authority
- HTTP remains available with `--http`
- no LAN sharing
- no custom TLDs
- no project config files
- no browser auto-open by default

## License

Apache-2.0
