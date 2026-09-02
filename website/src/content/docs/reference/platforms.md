---
title: Engines and platforms
description: The optimizing compiler, the portable interpreter, and exactly what CI covers.
---

`wazy.NewRuntime(ctx)` picks the optimizing compiler on amd64 and arm64 and falls back to the
interpreter everywhere else. Force either explicitly:

```go
r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigInterpreter())
r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
```

The compiler translates each module to machine code during `CompileModule` — 4x to 45x faster than
the interpreter across the go-anydoc runs. The interpreter has no architecture-specific code, so it
runs anywhere Go does, including `js/wasm` and `wasip1/wasm`.

## CI coverage

| | |
| --- | --- |
| **Full suite + spec corpora, every commit** | `linux/amd64`, `windows/amd64`, `darwin/arm64`, on two Go versions, plus a `-race -short` run and four fuzz targets — three differential against the interpreter, one for validation. |
| **Test binaries run, every commit** | `linux/arm64` and `linux/amd64` in a `scratch` container (one variant with `PROT_EXEC` denied); FreeBSD, OpenBSD, NetBSD, DragonFly and illumos on amd64. The scratch runs exclude the spectest packages; the BSD and illumos runs also exclude `imports/**`, `sysfs` and the generated 3.0 corpus. |
| **Cross-compiled every commit** | `plan9`, `js/wasm`, `wasip1/wasm`, `aix/ppc64`, `linux/{s390x,ppc64le,arm,386}`, `freebsd/amd64` — they build and run the interpreter; CI does not execute their suites. `riscv64` runs locally under `qemu-riscv64-static`, because the hosted runner never completes. |

If you depend on a target in the lower rows, [open an issue](https://github.com/samyfodil/wazy/issues)
and ask for CI coverage.

## 32-bit hosts

Linear memory tops out just under 2 GiB rather than the 4 GiB a 32-bit Wasm memory may declare,
bound by what a Go slice holds. A module asking for more is rejected, which the specification
allows.

## `PROT_EXEC`-denied environments

The compiler needs to map executable pages. Where that is denied — some hardened containers — the
interpreter is the fallback, and CI runs a `scratch` variant with `PROT_EXEC` denied to keep that
path honest.

## Wasm feature levels

```go
wazy.NewRuntimeConfig().WithCoreFeatures(api.CoreFeaturesV3)
```

`api.CoreFeaturesV2` is the default. `api.CoreFeaturesV3` turns on all eight proposals Wasm 3.0
folded in: tail calls, extended const, exception handling, typed function references, relaxed SIMD,
multiple memories, memory64 and garbage collection. Individual features can be composed with the
`api.CoreFeature*` bits, and `experimental.CoreFeaturesThreads` adds shared memory and atomics.
