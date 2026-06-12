# Contributing to acompose

Thanks for helping! This is a small, focused codebase — most contributions
are a single-file change.

## Dev setup

```sh
git clone https://github.com/htlin222/acompose && cd acompose
make check        # gofmt + vet + tests + dry-run integration — must stay green
```

Go 1.24+. You do **not** need Apple's `container` CLI for most development:
`--dry-run` prints the exact commands instead of executing them, and the test
suite never touches the real binary (non-dry paths are tested against a fake
`container` script on PATH). You only need real hardware (macOS 26+, Apple
Silicon, `container` CLI) to verify end-to-end behavior.

## The two rules of this codebase

1. **All `container` subcommand construction lives in one place** — the
   `ctr` / `runCmd` / `buildCmd` / `hostsInjectCmd` helpers in `src/main.go`.
   The platform CLI is young and renames flags between releases; centralizing
   makes that a one-line fix. Don't scatter `exec.Command("container", ...)`.
2. **Loud about platform gaps, silent about nothing.** Anything the runtime
   can't honour gets a `warn(...)` — never silently dropped. Benign,
   well-understood failures get one dim warning, not a raw error dump.

Also: compose-spec semantics are delegated to
[compose-go](https://github.com/compose-spec/compose-go) — never hand-parse
compose files; all execution goes through `runner.run` so `--dry-run` keeps
working; map iteration is sorted before generating commands (output must be
deterministic — CI greps it).

## Tests

```sh
make test                 # unit tests (also run with -race in CI)
make dryrun               # the integration test CI runs, locally
```

PRs that change command construction or output text should update both the
unit tests and the assertions in `.github/workflows/ci.yml`.

## Reporting bugs

Please include `acompose version`, `container --version`, your compose file
(or a minimal reproduction), and the output of `acompose up --dry-run` —
that's usually enough to pinpoint a translation bug without your hardware.
