# `greet` in JavaScript

A JavaScript plugin compiled to a WASI 0.2 component and called from a Go host.

## Install

```bash
npm i -g @bytecodealliance/jco @bytecodealliance/componentize-js @bytecodealliance/preview2-shim
```

`preview2-shim` is not optional with a **global** install: `componentize-js`
resolves it as a sibling, and `npm i -g` of the first two alone leaves it
missing, so `jco componentize` fails with:

```
Error [ERR_MODULE_NOT_FOUND]: Cannot find package '@bytecodealliance/preview2-shim'
```

(A local `npm i` in a project dir gets it as a transitive dependency and needs
no extra package — but then `node_modules` lands next to your source.)

## Build

```bash
jco componentize greet.js --wit greet.wit -o greet.wasm
```

## Toolchain used

| tool | version |
| --- | --- |
| node | v23.11.1 |
| npm | 10.9.2 |
| `@bytecodealliance/jco` | 1.32.1 |
| `@bytecodealliance/componentize-js` | 0.22.0 |
| `@bytecodealliance/preview2-shim` | 0.23.0 |

## Calling it

The world exports `greet` at the top level, so there is no interface prefix —
`inst.Call(ctx, "greet", "wazy")`, not `CallExport`. Confirm with:

```bash
wasm-tools component wit greet.wasm
```

```wit
world root {
  import wasi:io/error@0.2.10;
  ...
  export greet: func(name: string) -> string;
}
```

**This component needs WASI 0.2.** Unlike a hand-written Rust or C guest, it
embeds a whole JavaScript engine (StarlingMonkey/SpiderMonkey), and the engine
reaches for `wasi:clocks`, `wasi:random` and stdio while initialising — so the
host must pass `wasip2.With(...)`:

```go
inst, err := component.Instantiate(ctx, r, greetWasm, wasip2.With(wasip2.Config{})...)
```

Without it, the engine traps on the first unresolved import
(`wasi:clocks/monotonic-clock.now not implemented (trap stub)`).

## Why the .wasm is 12 MB

That is the JavaScript engine, not the program — `greet.js` is four lines. The
size is flat: a real plugin costs the same 12 MB plus its own source. Two ways
to trim it, both with trade-offs:

- `jco componentize greet.js --wit greet.wit -d all -o greet.wasm` disables every
  WASI feature. The result imports **nothing** (verified: it runs on wazy with no
  `wasip2` options at all) and saves ~40 KB. Cheap, but `console.log` is gone.
- `--backend quickjs` swaps SpiderMonkey for QuickJS and drops the component to
  **1.5 MB**. wazy cannot instantiate that output today — see below.

## Known gap: `--backend quickjs` does not run on wazy

```
component/instance: component instance 20 arg "local:init/module-loader"
references instance 19, which is not a prior nested instantiation
```

The QuickJS backend emits a `wac`-composed component: its component-instance
index space has 21 entries, but only two of them (18 and 20) are nested
*instantiations* — the rest are imported or aliased instances. Instance 20 is
then handed instance 19 as an instantiate-arg. wazy's graph engine resolves an
instance-sort instantiate-arg only against nested components it instantiated
itself, so the lookup misses and instantiation fails.

The default StarlingMonkey backend (what the build command above uses) is not
affected.

## The built `.wasm` is not committed

This guest embeds a whole language runtime, so the component is 12–18 MB. Committing
it would put ~90 MB into git history permanently, for a binary anyone can reproduce
with the one command above. Build it locally; the surrounding harnesses pick it up
from this directory.
