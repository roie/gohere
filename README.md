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

### Windows with WSL2

Install the npm package in WSL and run `gohere` normally. You do not need to install the npm package again in PowerShell. The WSL package includes a version-matched Windows companion that installs or reuses one Windows router for the current Windows user.

Initial WSL setup stays in the same WSL shell. It may request `sudo` once to allow the loopback edge to use ports 80/443 and to trust the Windows router's public CA certificate in Linux. Windows also shows its standard one-time security confirmation before trusting `gohere local development CA`; approve only that certificate. It does not add a Windows firewall rule, copy the Windows admin token into WSL, or start a second WSL router.

Mirrored networking uses the verified Windows loopback router directly. Separate-loopback/NAT networking uses a loopback-only WSL edge over a persistent Windows stdio helper. Windows still owns the router, routes, admin token, and CA private key; WSL owns only its Linux project processes and edge.

A WSL binary installed with `go install` can reuse a compatible `gohere.exe` already available on the Windows `PATH`, but it cannot bootstrap Windows by downloading another binary. Use the npm distribution for WSL-first installation.

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

Planned external servers receive `HOST`, `PORT`, and `GOHERE_URL` before startup. `GOHERE_URL` is the current service's final collision-safe public URL.

When one run starts multiple services, every child receives the same self-inclusive service map:

```text
GOHERE_WEB_URL=https://web.myrepo.localhost
GOHERE_WEB_TARGET=http://127.0.0.1:48101
GOHERE_WEB_PORT=48101

GOHERE_API_URL=https://api.myrepo.localhost
GOHERE_API_TARGET=http://127.0.0.1:48102
GOHERE_API_PORT=48102
```

Use `GOHERE_<NAME>_URL` for application configuration. `TARGET` is the exact direct upstream stored in the route, including the Windows-reachable endpoint in WSL. `PORT` is parsed from that exact target. Each child also receives its own `GOHERE_URL`, `HOST`, and `PORT`.

Static folders and files launch no external child, so they receive no runtime environment. Explicit task-like scripts such as `build`, `lint`, and `test`, plus raw commands without routing options, start lazily without router setup or a guessed `GOHERE_URL`. If a lazy command later exposes a server, gohere registers it and prints the final URL, but the already-running child is not retroactively modified.

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

Use `--http` when a route must stay on HTTP. It makes HTTP the advertised and canonical scheme for that route and disables automatic HTTP upgrading; the shared HTTPS listener remains available.

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

`gohere list --json` returns the same route snapshot in a stable machine-readable format, including route `id`, `generation`, lifecycle `state`, service, preferred public URL, exact target, and parsed port.

`gohere stop` stops routes for the current folder. `gohere stop <target>` stops a listed route by host, short host label, route name, or project name. `gohere stop --all` stops safely controllable routes and skips unverified live routes.

Route status can be `starting`, `ready`, `dead`, or `unknown`. `prune` removes routes that are confidently dead or whose reservation/lease has expired.

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

HTTP requests to HTTPS routes receive a temporary upgrade. Cross-origin requests that require preflight must target `https://` directly, and WebSocket clients must target `wss://` directly.

Each project gets a hidden local port. The service maps the clean `.localhost` hostname to that port using the request `Host` header.

First-time setup installs the local service in `~/.gohere/`, installs a local trusted certificate authority, and starts the service in the background. After that, `gohere` only starts your project and registers its route.

The service only serves local machine traffic. On macOS, gohere uses a port `80` listener that rejects non-loopback connections before requests reach the router.

State is stored in:

```text
~/.gohere/
```

Linux may ask for one-time permission to bind local ports and trust the local certificate authority.

When used from WSL2, `gohere` installs or reuses the current user's Windows authority automatically. Normal setup and recovery do not require switching to PowerShell.

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
- WSL1 is not supported
- the first WSL2 release permits one active integrated distribution/Linux user per Windows authority

## License

Apache-2.0
