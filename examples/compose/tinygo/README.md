# Cross-language composition: TinyGo

Both halves of the `wazy:compose/greeter` contract ([`greeter.wit`](greeter.wit)),
written in Go and compiled to WebAssembly components by TinyGo:

| file | world | what it is |
| --- | --- | --- |
| [`provider.wasm`](provider.wasm) | `provider` | exports `wazy:compose/greeter@0.1.0` |
| [`consumer.wasm`](consumer.wasm) | `consumer` | imports `wazy:compose/greeter@0.1.0`, exports `run` |

```
greet({name: "wazy", id: 42})  -> "Hello, wazy #42! (from TinyGo)"
greet-all(["a", "b"])          -> ["a (via TinyGo)", "b (via TinyGo)"]
greet-all([])                  -> []
```

The consumer hardcodes none of that. Every string `run` returns came back across
the interface from whatever provider it is composed with, so a wrong lift or
lower shows up in the output instead of being masked:

```
run() -> [ greet({name: "wazy", id: 42}),
           greet-all(["a", "b"])[0],
           "empty-len=" + len(greet-all([])) ]
```

`greet-all` builds one element per input element and does not special-case the
empty list -- a zero-length `list<string>` round trip is one of the things this
example is here to test.

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

Both worlds compile against one WIT package: the contract, the two build worlds,
and the WASI 0.2 dependency tree that ships inside TinyGo. Assemble it once and
build twice, changing only `--wit-world` and the package directory.

```sh
WIT="$(mktemp -d)"
cp greeter.wit build.wit "$WIT/"
mkdir -p "$WIT/deps/cli"
cp "$(tinygo env TINYGOROOT)"/lib/wasi-cli/wit/*.wit "$WIT/deps/cli/"
cp -r "$(tinygo env TINYGOROOT)"/lib/wasi-cli/wit/deps/* "$WIT/deps/"

tinygo build -target=wasip2 --wit-package "$WIT" --wit-world provider-tinygo -o provider.wasm ./provider
tinygo build -target=wasip2 --wit-package "$WIT" --wit-world consumer-tinygo -o consumer.wasm ./consumer

rm -rf "$WIT"
```

Result: `provider.wasm` ~478 KB, `consumer.wasm` ~476 KB.

### Why the extra worlds

[`build.wit`](build.wit) declares `provider-tinygo` and `consumer-tinygo`, which
are the two contract worlds plus `wasi:cli/imports`. TinyGo's `syscall` package
initialises itself on every wasip2 binary -- it reads the environment, the
initial working directory and the preopened directories -- so the core module
imports ~33 WASI 0.2 functions no matter what the guest code does, and
`wasm-tools component new` refuses a module whose imports the world does not
declare.

That is a property of the TinyGo runtime, not of this example: no build flag
(`-gc=leaking -scheduler=none -panic=trap -no-debug`) removes those imports.
`greeter.wit` stays the byte-identical contract, and the components still export
and import exactly what it says:

```sh
$ wasm-tools component wit provider.wasm | grep 'wazy:compose'
  export wazy:compose/greeter@0.1.0;

$ wasm-tools component wit consumer.wasm | grep -e 'wazy:compose' -e 'export run'
  import wazy:compose/greeter@0.1.0;
  export run: func() -> list<string>;
```

### Regenerating `gen/`

Only needed if `greeter.wit` changes. Two worlds, two output roots -- both
generate a package named `greeter`, so they cannot share one.

```sh
go install go.bytecodealliance.org/cmd/wit-bindgen-go@v0.7.0

wit-bindgen-go generate --world provider --out gen/provider \
  --package-root github.com/samyfodil/wazy/examples/compose/tinygo/gen/provider ./greeter.wit
wit-bindgen-go generate --world consumer --out gen/consumer \
  --package-root github.com/samyfodil/wazy/examples/compose/tinygo/gen/consumer ./greeter.wit
```

The generated code imports `go.bytecodealliance.org/cm`, pinned here to v0.3.0
(what `wit-bindgen-go` v0.7.0 generates against). Do not `go get ...@latest`:
v0.7.0 of that module no longer contains the root `cm` package.

## Running the halves on wazy

Each component runs on wazy on its own. The guests need WASI, so instantiate
with `wasip2.With`; wazy implements `wasi:cli/environment.initial-cwd`, which
TinyGo's `syscall` init calls during `_initialize`, so no host shim is needed.

```go
inst, _ := component.Instantiate(ctx, r, providerWasm, wasip2.With(wasip2.Config{})...)
out, _ := inst.Call(ctx, "wazy:compose/greeter@0.1.0#greet",
    []component.Value{"wazy", uint32(42)})       // "Hello, wazy #42! (from TinyGo)"
```

The consumer runs the same way with the two greeter functions supplied as host
imports (`component.WithImportCustom`), which is the useful way to exercise it
without a second component.

## Composing the two

`wasm-tools compose consumer.wasm -d provider.wasm -o self.wasm` produces a valid
component -- but **wazy cannot instantiate it today**. Details, including a
26-line reproducer that involves neither TinyGo nor `wasm-tools compose`, are in
the report accompanying this example; the short version is that a composed
component passes the OUTER component's *imported* instances straight through to
its nested components:

```wat
(instance (;11;) (instantiate 1
    (with "wasi:cli/environment@0.2.0" (instance 0))   ;; instance 0 is an IMPORT
    ...
```

and wazy's `instantiateNestedInstances` resolves an instance-sort instantiate
arg only against earlier *nested instantiations*:

```
component/instance: component instance 11 arg "wasi:cli/environment@0.2.0"
references instance 0, which is not a prior nested instantiation
```

This is a wazy gap, not a TinyGo one. Both `.wasm` files here are correct: they
validate under `wasm-tools validate --features all`, they show the right world,
and both run on wazy individually.
