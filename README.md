# gohere

Tiny local dev URL launcher for `.localhost` projects.

```text
http://myproject.localhost
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

In a package project, this runs the nearest `package.json` `dev` script. In a workspace root, this discovers workspace packages from `pnpm-workspace.yaml` or `package.json` workspaces, starts each package with a `dev` script, and gives each package its own route:

```text
gohere web    -> http://web.myrepo.localhost
gohere worker -> http://worker.myrepo.localhost
```

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

Use `GOHERE_<NAME>_URL` for app config. `PORT` and `TARGET` are advanced values and are only set when `gohere` controls that service port.

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

Open the project URL in your browser:

```bash
gohere --open
```

For static folders, `gohere` serves `index.html`. You can also open a specific file, for example `gohere about.html`, which routes to `http://myproject.localhost/about.html`.

CSS, images, and scripts are served normally.

## Examples

```text
myproject      -> http://myproject.localhost
@scope/web     -> http://web.localhost
./apps/web     -> http://web.repo.localhost
./dist         -> http://dist.localhost
about.html     -> http://myproject.localhost/about.html
```

## Route management

```bash
gohere list
gohere list --verbose
gohere stop
gohere prune
gohere doctor
gohere service stop
gohere uninstall
```

`gohere list --verbose` shows host, target, status, PID, and working directory.

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

`gohere` runs a local service on HTTP port `80`.

Each project gets a hidden local port. The service maps the clean `.localhost` hostname to that port using the request `Host` header.

State is stored in:

```text
~/.gohere/
```

On Linux/WSL, first-time setup may ask for permission so the service can bind to local port `80`.

On Windows, first-time setup starts the local service directly on `127.0.0.1:80`.

When used from WSL, `gohere` reuses a running Windows service automatically.

## Platform support

Current target: Linux / WSL and Windows.

Planned: macOS.

The npm package currently targets Linux x64 and Windows x64.

## Limits

- HTTP only
- `.localhost` only
- no HTTPS
- no LAN sharing
- no custom TLDs
- no project config files
- no browser auto-open by default

## License

Apache-2.0
