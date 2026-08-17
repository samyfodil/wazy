# wasi:otel test fixtures

Two components, both built with real `wit-bindgen` so the bytes under test are
what a generated guest actually sends rather than what the host expects.

| fixture | WIT source | proves |
| --- | --- | --- |
| `otelguest.wasm` | [WebAssembly/wasi-otel](https://github.com/WebAssembly/wasi-otel) | all three interfaces, against the current proposal |
| `outerguest.wasm` | [bytecodealliance/opentelemetry-wasi](https://github.com/bytecodealliance/opentelemetry-wasi) | `outer-span-context`, the name the guest SDKs still generate |

The two WITs differ in one function: the proposal renamed
`outer-span-context` to `current-span-context` in April 2026, and the guest
SDKs have not followed yet. The host serves both, and the second fixture is
what proves it.

## Rebuilding

Needs the `wasm32-wasip2` target (`rustup target add wasm32-wasip2`).

```bash
cd otelguest   # or outerguest
cargo build --release --target wasm32-wasip2
cp target/wasm32-wasip2/release/otelguest.wasm ..
```

`target/` is ignored; the checked-in `.wasm` is the artifact the tests embed.

## Why not the SDK's own examples

`bytecodealliance/opentelemetry-wasi`'s `rust/examples` are Spin applications:
they depend on `spin-sdk`, so a component built from them imports Spin's world
as well as wasi:otel and cannot be instantiated without implementing all of it.
These fixtures use the same generator against the same WIT, with nothing but
wasi:otel and what the Rust runtime itself needs.
