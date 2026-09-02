# greet — Rust

The `wazy:examples/greeter` world, implemented in Rust with `cargo component`.

```wit
world greeter {
  export greet: func(name: string) -> string;
}
```

Source: [`src/lib.rs`](src/lib.rs) (13 lines). Contract: [`greet.wit`](greet.wit).

## Prerequisites

```sh
rustup target add wasm32-wasip2
cargo install cargo-component --locked
```

Versions used to build the committed `greet.wasm`:

| tool | version |
| --- | --- |
| rustc | 1.90.0 |
| cargo-component | 0.21.1 |
| target | `wasm32-wasip2` |

## Build

```sh
cargo component build --release --target wasm32-wasip2
cp target/wasm32-wasip2/release/greet.wasm greet.wasm
```

`cargo component` regenerates `src/bindings.rs` from `greet.wit` on every build,
so the generated bindings are not committed.

> cargo-component 0.21.1 also builds its default `wasm32-wasip1` target on the
> side and its closing line reads `Creating component
> target/wasm32-wasip1/release/greet.wasm`. Ignore it — the component you want
> is the `wasm32-wasip2` one named in the `cp` above.

## Notes

`greet` itself touches no WASI, but Rust's `std` links its panic and stdio
machinery unconditionally, so the component still *declares* `wasi:cli`,
`wasi:io`, `wasi:clocks` and `wasi:filesystem` imports:

```sh
wasm-tools component wit greet.wasm
```

Pass `wasip2.With(wasip2.Config{})` when instantiating so those imports are
backed by a real implementation.
