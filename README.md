# gohere

Tiny local dev URL launcher for `.localhost` projects.

```text
http://myproject.localhost
```

Run `gohere` inside a package project or static folder. It starts or serves the project on a hidden local port, routes a clean `.localhost` hostname to it, and prints the URL.

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

`gohere` supports package projects and static files.

Run the default `dev` script from the nearest `package.json`:

```bash
gohere
```

Run a named package script:

```bash
gohere dev:web
```

Run a raw command:

```bash
gohere -- npm run dev
```

Route to a known target port:

```bash
gohere --target 5173 -- npm run dev
```

For static folders, `gohere` serves `index.html`. You can also open a specific file, for example `gohere about.html`, which routes to `http://myproject.localhost/about.html`.

CSS, images, and scripts are served normally.

## Examples

```text
myproject      -> http://myproject.localhost
@scope/web     -> http://web.localhost
repo/apps/web  -> http://web.repo.localhost
about.html     -> http://myproject.localhost/about.html
```

## Route management

```bash
gohere list
gohere list --verbose
gohere stop
gohere clean
gohere doctor
gohere uninstall
```

`gohere list --verbose` shows host, target, status, PID, and working directory.

Route status can be `ready`, `dead`, or `unknown`. `clean` removes only routes that are confidently dead.

## Uninstall

Clean up the copied router binary and service before removing the npm package:

```bash
gohere uninstall
npm uninstall -g gohere
```

`gohere uninstall` removes the local router install and asks before deleting routes, logs, and token state.

## How it works

`gohere` runs a local router on HTTP port `80`.

Each project gets a hidden local port. The router maps the clean `.localhost` hostname to that port using the request `Host` header.

State is stored in:

```text
~/.gohere/
```

On Linux/WSL, first-time setup may ask for permission so the router can bind to local port `80`.

## Platform support

Current target: Linux / WSL.

Planned: macOS and native Windows.

The npm package currently targets Linux x64 first.

## Limits

- HTTP only
- `.localhost` only
- no HTTPS
- no LAN sharing
- no custom TLDs
- no project config files
- no browser auto-open yet

## License

Apache-2.0
