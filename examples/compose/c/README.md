# Cross-language composition in C

Both halves of the [shared `greeter` contract](greeter.wit), written in C and
compiled to two WASI 0.2 components:

| file | world | role |
| --- | --- | --- |
| `provider.wasm` | `provider` | **exports** `wazy:compose/greeter@0.1.0` |
| `consumer.wasm` | `consumer` | **imports** `wazy:compose/greeter@0.1.0`, exports `run` |

Either one composes against the other language's opposite half. The interface is
picked to stress the canonical ABI where two guest allocators meet: a record
with a string field, and `list<string>` in both directions, including the empty
list.

```wit
interface greeter {
  record visitor { name: string, id: u32 }
  greet:     func(who: visitor)        -> string;
  greet-all: func(names: list<string>) -> list<string>;
}
```

`provider.c` answers `Hello, <name> #<id>! (from C)` and `<name> (via C)`.
`consumer.c` hardcodes none of that: every byte of its first two results comes
back across the interface, and the third reports the length `greet-all([])`
actually returned rather than the length it was supposed to return.

## Install

- **[wasi-sdk](https://github.com/WebAssembly/wasi-sdk/releases) 34.0** — the
  `wasm32-wasip2` sysroot. Stock distro clang has no wasm sysroot and cannot
  build this.

  ```sh
  curl -L https://github.com/WebAssembly/wasi-sdk/releases/download/wasi-sdk-34/wasi-sdk-34.0-x86_64-linux.tar.gz | tar xz
  export WASI_SDK=$PWD/wasi-sdk-34.0-x86_64-linux
  ```

- **[wit-bindgen](https://github.com/bytecodealliance/wit-bindgen) 0.47.0** and
  **[wasm-tools](https://github.com/bytecodealliance/wasm-tools) 1.253.0**

  ```sh
  cargo install wit-bindgen-cli --version 0.47.0
  cargo install wasm-tools --version 1.253.0
  ```

## Build

```sh
wit-bindgen c --world provider --out-dir build/provider greeter.wit
wit-bindgen c --world consumer --out-dir build/consumer greeter.wit

$WASI_SDK/bin/clang --target=wasm32-wasip2 -O2 -mexec-model=reactor -Wl,--strip-all \
  -Ibuild/provider provider.c build/provider/provider.c \
  build/provider/provider_component_type.o -o provider.wasm

$WASI_SDK/bin/clang --target=wasm32-wasip2 -O2 -mexec-model=reactor -Wl,--strip-all \
  -Ibuild/consumer consumer.c build/consumer/consumer.c \
  build/consumer/consumer_component_type.o -o consumer.wasm
```

There is no separate `wasm-tools component new` step: wasi-sdk 34 links through
`wasm-component-ld`, so `--target=wasm32-wasip2` emits a component directly.

`-mexec-model=reactor` is what keeps these libraries rather than commands.
Without it clang links a `wasi:cli/run` export and drags in `wasi:cli`
imports neither half uses. As built, **`provider.wasm` has no imports at all and
`consumer.wasm` imports nothing but the greeter interface**, so neither needs
WASI configured on the host:

```console
$ wasm-tools component wit provider.wasm
world root {
  export wazy:compose/greeter@0.1.0;
}

$ wasm-tools component wit consumer.wasm
world root {
  import wazy:compose/greeter@0.1.0;
  export run: func() -> list<string>;
}
```

`build/` is disposable; regenerate it with the two `wit-bindgen` commands.

## What the C has to get right

Two rules govern both source files.

A component-model `string` is a `(ptr, len)` pair in linear memory, **not** a
nul-terminated C string, so every result is assembled with `memcpy` and explicit
lengths. Neither file calls `snprintf` — the `u32`-to-decimal helper is
hand-written specifically so that pulling in `stdio` does not drag WASI into the
import section.

Ownership runs one way at each boundary. A buffer stored into an export's `ret`
belongs to the host afterwards and is released by the generated `cabi_post_*`
with `free()`, so everything that escapes comes from `malloc` and nothing that
escapes is freed locally. In the other direction, the strings an *import*
returns are lifted into the consumer's own memory through `cabi_realloc`, which
makes them the consumer's to free — and its to *move*. `run()` moves them
straight into its result list instead of copying; `cabi_post_run` frees them
from there.

The empty list is not special-cased away in either file. `greet-all([])`
returns the pair `(null, 0)`, which the canonical ABI accepts: null is aligned
for every alignment, and no element is ever loaded from a zero-length list.

## Verification

```console
$ wasm-tools validate --features all provider.wasm && echo OK
OK
$ wasm-tools validate --features all consumer.wasm && echo OK
OK
```

Each half was then run on wazy on its own — the provider called directly, the
consumer with the greeter import satisfied by a Go host function, so that its
output is distinguishable from the C provider's:

```console
provider greet(wazy,42)  = "Hello, wazy #42! (from C)"
provider greet-all(a,b)  = ["a (via C)" "b (via C)"]
provider greet-all([])   = [] (len 0)
consumer run[0]          = "Hello, wazy #42! (from HostGo)"
consumer run[1]          = "a (via HostGo)"
consumer run[2]          = "empty-len=0"
```

Both halves are correct, in both directions, including the empty-list path.

## The self-compose smoke test does not run yet

Composing the two into one component works:

```console
$ wasm-tools compose consumer.wasm -d provider.wasm -o self.wasm
composed component `self.wasm`
$ wasm-tools validate --features all self.wasm && echo VALID
VALID
$ wasm-tools component wit self.wasm
world root {
  export run: func() -> list<string>;
}
```

Instantiating that composed component on wazy does not:

```console
$ go run .
2026/09/01 17:22:32 instantiate: component/instance: component instance 2 arg
"wazy:compose/greeter@0.1.0" references instance 1, which is not a prior nested
instantiation
exit status 1
```

This is a wazy gap, not a C one — see the note below. It is unrelated to the
record, the lists, or the language: it is the *shape* every composition tool
emits at the top level.

## Toolchain versions

| tool | version |
| --- | --- |
| wasi-sdk | 34.0 (`clang 23.1.0-wasi-sdk`) |
| wit-bindgen | 0.47.0 |
| wasm-tools | 1.253.0 |
| Go (host) | 1.26.0 |
