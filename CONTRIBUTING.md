# Contributing

Contributions are welcome, from humans and AI agents alike. If the change is good and the tests are green, it goes in. Open a pull request.

## Before you open a PR

- Run `make check` for format and lint. `make format` fixes formatting.
- Run `make test`. wazy uses standard Go table-driven tests and an internal [require](./internal/testing/require) helper for assertions.
- If you touch the native compiler backend, the interpreter, or anything that affects generated code, run the spectests, and verify across engines/arches so a change that passes on one does not silently break another. Helpers: `make test.arm64` (arm64 compiler under qemu-user), `make test.interp` (the interpreter engine, via a riscv64 cross-run since a non-amd64/arm64 arch auto-selects it). Two gotchas these targets handle for you:
  - **arm64 under qemu-user needs `-one-insn-per-tb`.** qemu-user (through at least 8.2.2) has a multi-instruction translation-block self-modifying-code bug that SIGSEGVs `QEMU internal SIGSEGV {code=MAPERR, addr=0x20}` **~30% flaky** when running wazy's JIT'd code (it writes then executes machine code). The crash is inside qemu's own translator (not wazy — amd64 is 100% green and the arm64 codegen goldens pass, since they compare bytes without executing). `-one-insn-per-tb` makes SMC detection reliable at the cost of slower emulation.
  - **Compiler-codegen tests must skip where there is no compiler** (any arch but amd64/arm64): guard with `if !platform.CompilerSupported() { t.Skip(...) }`, or the first compile panics `unsupported architecture` (`isa_other.go`) and kills the whole package binary. The interpreter cross-run needs no qemu flag (no JIT).

## What makes a good PR

- A title that says what the change does.
- A short description of what changed and why. Skip it only for trivial fixes.
- If you claim a speedup, measure it on a quiet machine and put the numbers in the PR. Under system load, small real wins hide in the noise and small regressions look like wins.

That is the whole process.
