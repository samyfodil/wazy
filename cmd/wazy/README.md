## wazy CLI

The wazy CLI can be used to execute a standalone WebAssembly binary.

### Installation

```bash
$ go install github.com/samyfodil/wazy/cmd/wazy@latest
```

### Usage

The wazy CLI accepts a single argument, the path to a WebAssembly binary.
Arguments can be passed to the WebAssembly binary itself after the path.

```bash
wazy run calc.wasm 1 + 2
```

In addition to arguments, the WebAssembly binary has access to stdout, stderr,
and stdin.
