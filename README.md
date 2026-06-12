# acompose

[![ci](https://github.com/htlin222/acompose/actions/workflows/ci.yml/badge.svg)](https://github.com/htlin222/acompose/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/htlin222/acompose)](https://github.com/htlin222/acompose/releases/latest)
[![brew](https://img.shields.io/badge/brew-htlin222%2Ftap%2Facompose-orange)](https://github.com/htlin222/homebrew-tap)
[![license](https://img.shields.io/github/license/htlin222/acompose)](LICENSE)

Run your existing `docker-compose.yml` on [Apple's `container`](https://github.com/apple/container) —
the macOS-native, VM-per-container runtime — without rewriting it.

![acompose demo](assets/demo.gif)

Apple `container` gives every container its own lightweight VM with its own
real IP. That's great for isolation, but it ships with no Compose support,
which is the single biggest blocker for anyone coming from Docker. acompose
fills that gap, and leans into the IP-first model instead of fighting it.

## What it does

- Parses your compose file with the **official
  [compose-go](https://github.com/compose-spec/compose-go)** — the same
  library Docker Compose uses. `${VAR}`/`$$` interpolation, `.env`,
  `env_file`, override-file merging, port ranges, profiles: all handled by
  the spec implementation, not by hand-rolled parsing.
- Starts services in **dependency order** (`depends_on`, cycles rejected).
- Approximates **`condition: service_healthy`** by TCP-polling the
  dependency's first published port (the platform cannot run exec-style
  healthchecks).
- Wires **service-name DNS**: every container's name→IP goes into every
  peer's `/etc/hosts`, immediately and bidirectionally, so unmodified app
  code that connects to `db:5432` just works. `<SERVICE>_HOST` env vars are
  always injected as a fallback for shell-less (distroless/scratch) images.
- Publishes `ports:` to the host so `localhost:8080` keeps working.
- Supports **named volumes** natively (`container volume create`/`delete`,
  removed on `down -v`, kept otherwise — same contract as docker-compose).
- Applies **`deploy.resources.limits`** (cpus, memory) as real **VM-level
  allocation** — not a cgroups share; whole CPUs, rounded up loudly.
- **`acompose dev`** — compose-spec `develop.watch` hot reload: `sync` copies
  changed files into the container, `rebuild` rebuilds and recreates,
  `restart` bounces — with debounce, ignore rules, and a poll watcher with
  zero extra dependencies.
- **`acompose dns`** — host-side DNS names via the runtime's native
  `container system dns` (the `*.orb.local` idea): one-time setup, then
  `<container>.<project>` resolves from your browser to the container's
  real IP.
- **`acompose watch`** — a built-in supervisor that honours `restart:`
  policies the runtime itself doesn't enforce: polls the stack, restarts
  exited services, and re-wires /etc/hosts when IPs change (the
  [autoheal](https://github.com/willfarrell/docker-autoheal) idea, native).
- **`acompose update`** — pulls newer images, recreates only the services
  whose manifest digest actually moved, rebuilds `build:` services (the
  [dockcheck](https://github.com/mag37/dockcheck) idea, native).
- **`acompose ui`** — a live dashboard in the same binary: every service as
  a card with its real IP front and center, status lamp, published ports,
  logs panel, stop/start. `acompose stats` for live resource usage.
- Is **loud about everything the platform can't honour** (deploy limits,
  exec healthchecks, x86 images). No silent surprises.

## Install

```sh
brew install htlin222/tap/acompose

# or from source:
make install       # builds and installs into your brew prefix (or /usr/local)
```

Requires macOS 26 with Apple's `container` CLI installed; Apple Silicon only.

## Use

No compose project handy? Get a working demo in two commands:

```sh
mkdir demo && cd demo
acompose init            # scaffolds a minimal docker-compose.yml
acompose up              # then open http://localhost:8080
```

In a real project:

```sh
cd your-project
acompose check           # compatibility report — how well does this file translate?
acompose up --dry-run    # see the exact `container` commands first
acompose up
acompose ui              # live dashboard on http://127.0.0.1:4242
acompose dev             # develop.watch hot reload: sync / rebuild / restart
acompose watch           # supervise restart: policies (poll, restart, re-wire DNS)
acompose update          # pull newer images, recreate only what changed
acompose dns setup       # host DNS names via container system dns (one-time sudo)
acompose ps
acompose stats
acompose logs api -f
acompose exec api -- sh
acompose refresh         # after sleep/wake or restarts: re-grab IPs, rewrite hosts
acompose down            # add -v to also remove named volumes
```

## Reverse proxy (nginx & friends)

Apps that resolve peer names **while booting** — nginx parsing
`proxy_pass http://backend` is the classic — race acompose's `/etc/hosts`
injection and exit with *"host not found in upstream"* (the runtime
regenerates `/etc/hosts` on every boot, so a restart can't win the race
either). The supported pattern is the `<SERVICE>_HOST` env var acompose
always injects, which the official nginx image consumes natively via
envsubst templates:

```yaml
services:
  backend:
    image: traefik/whoami
  proxy:
    image: nginx:alpine
    depends_on: [backend]          # ensures BACKEND_HOST is known at start
    ports: ["8080:80"]
    volumes:
      - ./default.conf.template:/etc/nginx/templates/default.conf.template
```

```nginx
# default.conf.template — envsubst fills BACKEND_HOST at container start
server {
  listen 80;
  location / { proxy_pass http://${BACKEND_HOST}; }
}
```

Verified end-to-end on the real runtime. Apps that connect *after* startup
(databases clients, app servers) don't need any of this — plain service
names work.

## Switching from Docker Desktop or OrbStack?

The evaluation funnel, in order — the first two don't touch anything:

```sh
acompose doctor          # is this machine ready? (arch, macOS, CLI version, service)
acompose check           # point it at your compose file: what translates, what won't
acompose import-volumes  # copy your named-volume DATA across (postgres comes with you)
acompose up
```

`import-volumes` streams each volume `docker run … tar -cf - | container run …
tar -xf -`, so Docker/OrbStack must still be installed when you run it — it
refuses to overwrite non-empty target volumes and warns if a docker container
is still using the source.

`--file F` (repeatable) and `-p NAME` work like you'd expect;
`docker-compose.override.yml` is auto-merged.

## Honest limitations

Platform-level gaps a CLI wrapper cannot fix — all warned at startup:

- exec-style healthchecks (TCP polling is the approximation)
- `restart:` policies are not enforced by the runtime itself
  (`acompose watch` supervises them from the outside instead)
- `deploy:` resource limits
- x86 (`platform: linux/amd64`) images are not seamless on this runtime
- CI parity: your CI runs real Docker Compose; behavior can differ

## Tested against

| acompose | Apple `container` CLI | macOS | hardware |
| -------- | --------------------- | ----- | -------- |
| v0.1.x   | 1.0.0                 | 26.2  | Apple Silicon (arm64) |

The `container` CLI is young and its flags shift between releases. Every
subcommand acompose issues is constructed in one place (`ctr`/`runCmd`/
`buildCmd` in `src/main.go`), so adapting to a renamed flag is a one-line fix —
if a newer CLI broke something, [open an issue](https://github.com/htlin222/acompose/issues)
with your `container --version`.

## FAQ

**vs OrbStack?** Different layer and a different bet. OrbStack is a complete
Docker environment (one shared Linux VM, full Docker API) — if it works for
you today, keep it. acompose is the Compose layer for Apple's OS-native
runtime: VM-per-container kernel isolation, a real IP per container, zero
third-party runtime dependency, all MIT/free. Same compose file either way —
`acompose check` tells you how yours translates, `acompose import-volumes`
brings your data across.

**Why not Podman?** There's no gap to fill: Podman has
[podman-compose](https://github.com/containers/podman-compose), and its
Docker-compatible API socket means plain `docker compose` works against it
too. acompose exists only because Apple's `container` has no Compose support.
Podman on macOS is also a single shared Linux VM — none of the
VM-per-container, IP-first properties this tool is built around apply there.

**Why not Kubernetes?** Different abstraction, and the road is blocked at the
platform level anyway: compose→manifest conversion is
[kompose](https://kompose.io)'s job, and using Apple `container` as a k8s node
runtime would require a CRI implementation, which doesn't exist. If that ever
changes, this answer gets revisited.

**So what IS portable?** Your compose file. acompose parses it with the
official compose-go and invents no syntax of its own — the same
`docker-compose.yml` runs via acompose on your Mac, real Docker Compose in CI,
podman-compose on a Podman box, or `kompose convert` on the way to a cluster.
The runner is per-platform; the file is the contract, and acompose will never
hold it hostage.

## Repo layout

- `src/main.go`, `src/ui.go` — the Go implementation (the real one);
  `src/main_test.go` — unit tests
- `src/prototype/acompose.py` — the original Python prototype, kept for
  history; it hand-implements the compose spec and is superseded by compose-go
- `examples/` — a small stack exercising interpolation, healthcheck
  conditions, build, and published ports: `make dryrun`
- `.github/workflows/` — CI (gofmt, vet, test, cross-build, and an
  integration test asserting on `--dry-run` output) and a tag-triggered
  release
- `.goreleaser.yaml` — multi-platform release binaries on `git tag v*`

## License

MIT
