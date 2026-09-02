---
title: CLI
description: The standalone wazy runner — the same binary the wasi-testsuite runs against.
---

A standalone runner ships in the box. It takes core modules and WASI 0.1; components go through the
library.

```bash
go install github.com/samyfodil/wazy/cmd/wazy@latest
```

Release binaries for Linux, macOS and Windows are built `CGO_ENABLED=0`.

## `wazy run`

```bash
wazy run app.wasm arg1 arg2
wazy run -mount=.:/:ro -env-inherit app.wasm
```

Arguments after the `.wasm` path go to the guest. The guest gets stdin, stdout and stderr.

| Flag | |
| --- | --- |
| `-mount=<host>:<guest>[:ro]` | Mount a host directory into the guest. Repeatable. `:ro` makes it read-only. |
| `-env=<key>=<value>` | Set one environment variable. Repeatable. |
| `-env-inherit` | Pass the process's whole environment through. |
| `-listen=<host>:<port>` | Pre-open a listening socket for the guest. Repeatable. |
| `-timeout=<duration>` | Kill the guest after this long, e.g. `-timeout=5s`. |
| `-interpreter` | Force the interpreter instead of the optimizing compiler. |
| `-cachedir=<dir>` | Persist compiled machine code here; reused on the next run. |
| `-hostlogging=<scopes>` | Log host calls. Scopes: `clock`, `filesystem`, `memory`, `proc`, `poll`, `random`, `sock`. |

`-hostlogging` is the fastest way to find out why somebody else's guest is failing — it prints
every WASI call the guest makes, with arguments.

## `wazy compile`

```bash
wazy compile -cachedir=/var/cache/wazy app.wasm
```

Compiles ahead of time into the cache directory, so the first `wazy run` against the same cache
starts without a compilation pause. Same key rules as the library's
[compilation cache](../../guides/caching/): module bytes, engine, features and wazy version.

## `wazy version`

```bash
wazy version
```

## Conformance

This binary is what the [wasi-testsuite](https://github.com/WebAssembly/wasi-testsuite) runs
against in CI — AssemblyScript, C and Rust suites, no skipped tests, on Linux, macOS **and**
Windows.
