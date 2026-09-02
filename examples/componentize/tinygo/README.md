# `greet` in TinyGo

The `wazy:examples/greeter` world (see [`greet.wit`](greet.wit)), implemented in
Go and compiled to a WebAssembly component by TinyGo.

```
greet("wazy") -> "Hello, wazy! (from TinyGo)"
```

[`main.go`](main.go) is the whole guest: it assigns one closure to the exported
function. Everything under [`gen/`](gen) is generated from `greet.wit` by
`wit-bindgen-go` and committed so the component builds without that step.

## Install first

| tool | version used | notes |
| --- | --- | --- |
| [Go](https://go.dev/dl/) | 1.26.0 | TinyGo compiles with the host Go toolchain |
| [TinyGo](https://github.com/tinygo-org/tinygo/releases) | 0.42.0 | `-target=wasip2` needs 0.33+ |
| [`wasm-tools`](https://github.com/bytecodealliance/wasm-tools) | 1.253.0 | must be on `PATH`; TinyGo shells out to it for `component embed` / `component new` |
| `wit-bindgen-go` | v0.7.0 | only to regenerate `gen/` |

```sh
# TinyGo (single binary tarball, no build step)
curl -LO https://github.com/tinygo-org/tinygo/releases/download/v0.42.0/tinygo0.42.0.linux-amd64.tar.gz
tar xzf tinygo0.42.0.linux-amd64.tar.gz && export PATH="$PWD/tinygo/bin:$PATH"

# wasm-tools
cargo install --locked wasm-tools@1.253.0
```

## Build

```sh
# The WIT package TinyGo compiles against: the contract, the build world, and
# the WASI 0.2 dependency tree that ships inside TinyGo.
WIT="$(mktemp -d)"
cp greet.wit build.wit "$WIT/"
mkdir -p "$WIT/deps/cli"
cp "$(tinygo env TINYGOROOT)"/lib/wasi-cli/wit/*.wit "$WIT/deps/cli/"
cp -r "$(tinygo env TINYGOROOT)"/lib/wasi-cli/wit/deps/* "$WIT/deps/"

tinygo build -target=wasip2 --wit-package "$WIT" --wit-world greeter-tinygo -o greet.wasm .
rm -rf "$WIT"
```

Result: `greet.wasm`, ~470 KB.

```sh
$ wasm-tools component wit greet.wasm | head -16
...
  export greet: func(name: string) -> string;
```

### Why the extra world

`build.wit` declares `greeter-tinygo`, which is `greeter` plus
`wasi:cli/imports`. TinyGo's `syscall` package initialises itself on every
wasip2 binary -- it reads the environment, the initial working directory and
the preopened directories -- so the core module imports ~33 WASI 0.2 functions
no matter what the guest code does, and `wasm-tools component new` refuses a
module whose imports the world does not declare.

That is a property of the TinyGo runtime, not of this example: no build flag
(`-gc=leaking -scheduler=none -panic=trap -no-debug`) removes those imports.
`greet.wit` stays the byte-identical contract; the component it produces still
exports exactly `greet: func(name: string) -> string`.

### Regenerating `gen/`

Only needed if `greet.wit` changes.

```sh
go install go.bytecodealliance.org/cmd/wit-bindgen-go@v0.7.0
wit-bindgen-go generate --world greeter --out gen \
  --package-root github.com/samyfodil/wazy/examples/componentize/tinygo/gen ./greet.wit
```

The generated code imports `go.bytecodealliance.org/cm`, pinned here to v0.3.0
(what `wit-bindgen-go` v0.7.0 generates against). Do not `go get ...@latest`:
v0.7.0 of that module no longer contains the root `cm` package.

## Running it on wazy

The guest needs WASI, so instantiate with `wasip2.With`:

```go
inst, err := component.Instantiate(ctx, r, greetWasm, wasip2.With(wasip2.Config{})...)
out, err := inst.Call(ctx, "greet", "wazy")   // "Hello, wazy! (from TinyGo)"
```

TinyGo's `syscall` init calls `wasi:cli/environment.initial-cwd` during
`_initialize`. wazy implements it (returning `none` unless you set
`WASIConfig.InitialCWD`), so no host-side shim is needed.
