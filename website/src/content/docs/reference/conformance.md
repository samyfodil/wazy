---
title: Conformance
description: Pass counts against somebody else's suite, with the skip list empty and a harness that fails if it changes.
---

Correctness here is a pass count against somebody else's suite, not a claim.

| Suite | Source | Result |
| --- | --- | --- |
| Core spec tests | [WebAssembly/testsuite](https://github.com/WebAssembly/testsuite) + the proposal repos | 13 corpora, **711 generated case files, no skip list**; the 3.0 corpus runs on **both** engines. |
| Component Model async | the official async `.wast` suites | **31 suites, 0 skipped, 0 failed.** |
| Canonical ABI values | the official `test/values` suites | **7 suites, 0 skipped, 0 failed**, through `wasm-tools json-from-wast`. Every vendored module instantiates — see below. |
| wasi-testsuite | [WebAssembly/wasi-testsuite](https://github.com/WebAssembly/wasi-testsuite) | AssemblyScript, C and Rust, **no skipped tests**, on Linux, macOS **and** Windows, through the `wazy` CLI. |
| Cross-proposal interaction | wasmtime's `tests/misc_testsuite` | 9 `.wast` vendored from wasmtime, beside 5 written here. |

```bash
go test ./internal/integration_test/spectest/...  # core spec corpora
go test ./internal/component/...                  # component ABI + async .wast
```

## The skip list is empty, and the harness fails if that changes

`wastKnownSkips` in `internal/component/instance/wast_conformance_test.go` is an empty map: every
vendored module of all 7 suites instantiates. The harness fails if the observed set differs in
**either** direction — a new entry is a regression, and a stale one means a gap closed and the list
must shrink. An empty list is therefore an assertion the test enforces, not a claim made here.

The last four entries — `types.11`, `fused.22`, `fused.23` and `resources.14` — were all the same
gap: a nested component naming a definition an *enclosing* component owns. Closing them meant
modelling the core-module and component index spaces separately (this package had equated each with
a single binary section), adding a decode-time outer link so `alias outer` resolves, accepting an
instance-sort instantiate argument that names an imported instance — plus, for fused.22's second
and unrelated half, masking flags to their label count on lift.

The mechanism exists because a silent skip once hid a bug: the harness used to only log a skip,
which made `buildPassthroughShim` rejecting an empty export name invisible — the official suite
reproduced the reporter's failure all along and still reported PASS
([#25](https://github.com/samyfodil/wazy/issues/25)).

## Beyond the suites

- A separate workflow runs the Zig standard-library test binary, the Go `wasip1` standard-library
  tests on two Go versions, and libsodium's suite, on every push.
- A differential trace-oracle byte-compares the async runtime against the specification's reference
  implementation (`definitions.py`).
- Four fuzz targets run in CI — three differential against the interpreter, one for validation.

One official async fixture is broken upstream; the 31/31 runs against a corrected copy, and the fix
is filed as [component-model#679](https://github.com/WebAssembly/component-model/pull/679).

## Wasm 3.0

There is no separate 3.0 suite in the specification repository — `WebAssembly/testsuite@main` *is*
it. wazy generates its corpus from that tree, and the 3.0 cases run on both the compiler and the
interpreter. The cross-proposal cases in
[`internal/integration_test/spectest/v3-interaction`](https://github.com/samyfodil/wazy/tree/main/internal/integration_test/spectest/v3-interaction)
named `wasmtime-*.wast` are vendored from wasmtime's `tests/misc_testsuite`; the `wazy-*.wast`
beside them are this repository's.
