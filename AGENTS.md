# AGENTS.md

Guidance for AI coding agents (and humans) working in this repository. wazy is a
WebAssembly runtime for Go.

## Project

- Module: `github.com/samyfodil/wazy`
- Pure Go, no CGO, zero third-party dependencies. This is a hard constraint — do
  not add dependencies or introduce cgo.
- Go floor 1.25 (`go.mod`); developed on Go 1.26.
- Derived from wazero. The goals are pure Go, performance, and conformance to the
  WebAssembly and WASI standards. We do not preserve wazero's API or architecture —
  change either when it serves those goals.

## Layout

- Root package (`runtime.go`, `config.go`, `builder.go`, `host_typed.go`, …) — the public API.
- `api/` — stable API types shared across the tree.
- `experimental/` — opt-in, unstable API.
- `imports/wasi_snapshot_preview1/` — WASI preview1 host module.
- `internal/engine/native/` — the optimizing compiler (amd64 + arm64 JIT).
- `internal/engine/interpreter/` — the portable interpreter (fallback where the compiler is unsupported).
- `internal/sysfs/` — host filesystem; `internal/wasip1/` — WASI wire layer.
- `cmd/wazy/` — the CLI.
- `internal/integration_test/` — spec, WASI, stdlib, and fuzz conformance suites.

## Build, test, format

Use the Makefile; it encodes the real invocations.

- `make test` — full test run. Root `go test ./...` does **not** cover the nested
  modules; `make test` also runs `internal/version/testdata` and
  `internal/integration_test/fuzz/wazylib`.
- `make check` — pre-flight for pull requests (builds on fallback platforms, runs
  spectests, etc.). Run this before proposing a PR.
- `make format` — gofumpt + gosimports + asmfmt. Stricter than `gofmt`; you **must**
  run it or the Pre-commit CI job fails. `.s` assembly is formatted by asmfmt, so
  plain `gofmt` is not sufficient.
- `make lint` — golangci-lint.
- `make coverage` — coverage over the main packages.

## Conventions & gotchas (learned the hard way)

- **Formatting:** always `make format` before committing — gofumpt/asmfmt catch what `gofmt` misses.
- **Nested modules:** `benchmarks/vs-wazero`, `internal/integration_test/fuzz`, and
  `internal/version/testdata` have their own `go.mod` and are invisible to a root
  `go test ./...`. Change and test them in-directory.
- **Fresh-checkout CI:** some integration suites (libsodium, stdlibs) download or
  compile fixtures into git-ignored `testdata/` dirs. Code that `//go:embed`s such a
  dir must sit behind a build tag, or a clean checkout won't compile.
- **Host functions:** use the generic, reflection-free `HostFunc0..8` / `HostProc0..8`
  in `host_typed.go`. Do not reintroduce `reflect`-based host dispatch.
- **arm64:** JIT changes must be verified on arm64. Under emulation:
  `GOARCH=arm64 CGO_ENABLED=0 go test ./internal/engine/native/...` (needs qemu-user).
- **wasip1 FS semantics:** the stdlib suite
  (`internal/integration_test/stdlibs/bench_test.go`) runs Go's own stdlib tests
  compiled to wasip1. A few fail on inherited sysfs limitations (path-based
  resolution) and are listed in `skip`. Add to that list, with a comment, when a
  failure is a known wasip1 gap — not a regression.

## Commits & PRs

- Conventional-commit prefixes, matching history: `feat:`, `fix:`, `test:`, `ci:`,
  `docs:`, `style:`, optionally with a package scope (e.g. `sysfs:`, `test(stdlibs):`).
- Keep diffs minimal; prefer deletion over addition.
- A green `make check` is the bar for merge.

## Docs map

- `README.md` — what wazy is and how to use it.
- `RATIONALE.md` — design decisions and internals.
- `CONTRIBUTING.md` — how to contribute (humans and agents welcome).
- `OPTIMIZATIONS.md` — performance roadmap and completed work.
