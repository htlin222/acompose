# acompose

Run your existing `docker-compose.yml` on [Apple's `container`](https://github.com/apple/container) —
the macOS-native, VM-per-container runtime — without rewriting it.

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

```sh
cd your-project
acompose up --dry-run    # see the exact `container` commands first
acompose up
acompose ui              # live dashboard on http://127.0.0.1:4242
acompose watch           # supervise restart: policies (poll, restart, re-wire DNS)
acompose update          # pull newer images, recreate only what changed
acompose ps
acompose stats
acompose logs api -f
acompose exec api -- sh
acompose refresh         # after sleep/wake or restarts: re-grab IPs, rewrite hosts
acompose down            # add -v to also remove named volumes
```

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

The `container` CLI is young and its flags shift between releases. Every
subcommand acompose issues is constructed in one place (`ctr`/`runCmd`/
`buildCmd` in `src/main.go`), so adapting to a renamed flag is a one-line fix.

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
