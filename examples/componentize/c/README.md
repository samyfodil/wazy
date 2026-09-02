# `greet` in C

The [shared `greeter` world](greet.wit), implemented in C and compiled to a
WASI 0.2 component.

```wit
world greeter {
  export greet: func(name: string) -> string;
}
```

`greet.c` defines exactly one function. `wit-bindgen` generates the canonical
ABI glue around it.

## Install

- **[wasi-sdk](https://github.com/WebAssembly/wasi-sdk/releases) 34.0** —
  provides the `wasm32-wasip2` sysroot. The stock distro clang cannot build
  this; it has no wasm sysroot.

  ```sh
  curl -L https://github.com/WebAssembly/wasi-sdk/releases/download/wasi-sdk-34/wasi-sdk-34.0-x86_64-linux.tar.gz | tar xz
  export WASI_SDK=$PWD/wasi-sdk-34.0-x86_64-linux
  ```

- **[wit-bindgen](https://github.com/bytecodealliance/wit-bindgen) 0.47.0**

  ```sh
  cargo install wit-bindgen-cli --version 0.47.0
  ```

## Build

```sh
wit-bindgen c --out-dir build greet.wit
$WASI_SDK/bin/clang --target=wasm32-wasip2 -O2 -mexec-model=reactor -Wl,--strip-all \
  -Ibuild greet.c build/greeter.c build/greeter_component_type.o -o greet.wasm
```

That is the whole build — there is no separate `wasm-tools component new` step.
wasi-sdk 34 links through `wasm-component-ld`, so `--target=wasm32-wasip2`
emits a component (13K) directly.

Two flags carry weight:

- `-mexec-model=reactor` builds a *library* component. Without it clang links a
  command: the component grows to 135K and picks up a `wasi:cli/run` export
  plus `wasi:cli/environment` and `wasi:cli/exit` imports it never uses.
- `-Wl,--strip-all` drops debug info and names, 51K to 13K.

Confirm the built component matches the contract:

```sh
wasm-tools component wit greet.wasm
```

```wit
world root {
  export greet: func(name: string) -> string;
}
```

No imports at all, so the host needs no WASI configuration.

## Notes

`build/` holds generated bindings and is disposable; regenerate it with the
`wit-bindgen` command above.

A component-model `string` is a `(ptr, len)` pair, not a nul-terminated C
string, so `greet.c` builds its result with `memcpy` and explicit lengths. The
returned buffer is `malloc`'d and handed to the host; the generated
`cabi_post_greet` frees it after the host has copied it out.

Verified against wazy with `wasm-tools 1.253.0`.
