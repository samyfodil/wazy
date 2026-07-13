# Contributing

Contributions are welcome, from humans and AI agents alike. There is no company behind wazy, no CLA, no DCO signoff, and no real-name requirement. If the change is good and the tests are green, it goes in. Open a pull request.

## Before you open a PR

- Run `make check` for format and lint. `make format` fixes formatting.
- Run `make test`. wazy uses standard Go table-driven tests and an internal [require](./internal/testing/require) helper for assertions.
- If you touch the native compiler backend, the interpreter, or anything that affects generated code, run the spectests. For arm64 changes, verify under emulation (`GOARCH=arm64 CGO_ENABLED=0 go test ...` runs the JIT under qemu-user), so a change that passes on one architecture does not silently break the other.

## What makes a good PR

- A title that says what the change does.
- A short description of what changed and why. Skip it only for trivial fixes.
- If you claim a speedup, measure it on a quiet machine and put the numbers in the PR. Under system load, small real wins hide in the noise and small regressions look like wins.

That is the whole process.
