## CLI example

This example shows a simple CLI application compiled to WebAssembly and
executed with the wazy CLI.

```bash
$ go run github.com/samyfodil/wazy/cmd/wazy run testdata/cli.wasm 3 4
```

The wazy CLI can run stand-alone Wasm binaries, providing access to any
arguments passed after the path. The Wasm binary reads arguments and otherwise
operates on the host via WASI functions.
