# `wazy:compose` in Python — provider and consumer

Both halves of the cross-language composition matrix, built with
[componentize-py](https://github.com/bytecodealliance/componentize-py):

| file | world | role |
|---|---|---|
| `provider.wasm` | `provider` | exports `wazy:compose/greeter@0.1.0` |
| `consumer.wasm` | `consumer` | imports `wazy:compose/greeter@0.1.0`, exports `run` |

Either one composes against another language's opposite half — the WIT in
`greeter.wit` is byte-identical across all six languages.

Both components embed a whole CPython interpreter, so each is ~18 MB.

## Install

The venv lives outside the repo so nothing lands in the working tree:

```sh
python3 -m venv ~/cpy-venv/.venv
~/cpy-venv/.venv/bin/pip install componentize-py==0.25.0
```

## Build

```sh
~/cpy-venv/.venv/bin/componentize-py -d greeter.wit -w provider componentize provider_app -o provider.wasm
~/cpy-venv/.venv/bin/componentize-py -d greeter.wit -w consumer componentize consumer_app -o consumer.wasm
```

`-w` picks the world; the trailing bare name is the Python module. The module
name must differ from the world name — componentize-py generates a `wit_world`
module for the world and Python cannot load two top-level modules with the same
name — hence `provider_app.py` / `consumer_app.py` rather than
`provider.py` / `consumer.py`.

componentize-py handles both directions of the contract natively, so nothing
here is hand-written ABI:

* an **exported** interface becomes `wit_world.exports.Greeter` (the protocol to
  implement, in a class named after the interface) plus
  `wit_world.exports.greeter` (holding the `visitor` record);
* an **imported** interface becomes `wit_world.imports.greeter`, a plain module
  with `greet()` / `greet_all()` functions to call.

## Compose

```sh
wasm-tools compose consumer.wasm -d provider.wasm -o self.wasm
```

(The `wasi:*` "will be imported because a dependency could not be found"
warnings are expected: neither half provides WASI, so it stays an import of the
composed component for the host to satisfy.)

## Toolchain used

| | |
|---|---|
| Python | 3.12.3 |
| componentize-py | 0.25.0 |
| wasm-tools | 1.253.0 |

## Host requirements

Same two as the single-function Python example:

```go
r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfig().
    WithCoreFeatures(api.CoreFeaturesV2|api.CoreFeatureExtendedConst))

inst, err := component.Instantiate(ctx, r, componentBytes, wasip2.With(wasip2.Config{})...)
```

* **`extended-const`** — componentize-py's CPython build initializes a global
  with `i32.add`, which the default `api.CoreFeaturesV2` rejects.
  `api.CoreFeaturesV3` works too.
* **WASI 0.2** — CPython calls `wasi:cli/environment` during interpreter
  start-up, before it reaches any export. The zero-value `wasip2.Config{}` is
  enough; no stdio or filesystem is needed.

## Verified

Both components validate, and their worlds are the contracted ones:

```
$ wasm-tools validate --features all provider.wasm    # ok
$ wasm-tools validate --features all consumer.wasm    # ok
$ wasm-tools component wit provider.wasm | grep wazy:compose
  export wazy:compose/greeter@0.1.0;
$ wasm-tools component wit consumer.wasm | grep -e wazy:compose -e 'export run'
  import wazy:compose/greeter@0.1.0;
  export run: func() -> list<string>;
```

Each half, run on wazy **on its own** — the provider called directly, the
consumer with the `greeter` import supplied by the Go host (deliberately worded
`GoHost`, so every string below is provably one that crossed the interface):

```
=== provider.wasm alone on wazy ===
  greet({wazy,42})   = "Hello, wazy #42! (from Python)"
  greet-all([a,b])   = ["a (via Python)" "b (via Python)"]
  greet-all([])      = [] (len 0)
=== consumer.wasm alone on wazy, greeter supplied by the Go host ===
  [0] Hello, wazy #42! (from GoHost)
  [1] a (via GoHost)
  [2] empty-len=0
```

The self-composed `self.wasm` is correct, but **wazy cannot yet instantiate it**
— see below. On wasmtime 37.0.1 it returns exactly what the contract asks for:

```
$ wasmtime run -S common --invoke 'run()' self.wasm
["Hello, wazy #42! (from Python)", "a (via Python)", "empty-len=0"]
```

## Known wazy gap: composed components do not instantiate

```
$ go run .   # host calling self.wasm
error: instantiate: component/instance: component instance 25 arg "wasi:io/poll@0.2.4" \
    references instance 0, which is not a prior nested instantiation
```

`instantiateGraph` (`internal/component/instance/graph.go:882`) resolves an
instance-sort instantiate arg only against `byIdx`, which holds *locally
instantiated nested components* (`Kind == 0x00`). Two other instance-space
entries can legally appear as an arg, and both are exactly what
`wasm-tools compose` emits:

1. an **imported** instance of the outer component (WASI, forwarded into each
   sub-component);
2. an **aliased** instance — `(alias export $provider "wazy:compose/greeter@0.1.0")`
   — which is how the composed component links the consumer to the provider at
   all.

Neither is Python-specific; it reproduces with no WASI and no guest language, in
a hand-written component:

```wat
(component
  (component $P
    (core module $m (func (export "f") (result i32) i32.const 7))
    (core instance $mi (instantiate $m))
    (func $f (result u32) (canon lift (core func $mi "f")))
    (instance $i (export "f" (func $f)))
    (export "p:p/i" (instance $i)))
  (instance $pi (instantiate $P))
  (alias export $pi "p:p/i" (instance $pia))
  (component $C
    (import "p:p/i" (instance $e (export "f" (func (result u32)))))
    (alias export $e "f" (func $f))
    (export "run" (func $f)))
  (instance $ci (instantiate $C (with "p:p/i" (instance $pia))))
  (alias export $ci "run" (func $run))
  (export "run" (func $run)))
```

```
component/instance: component instance 2 arg "p:p/i" references instance 1,
which is not a prior nested instantiation
```

This is reported, not worked around: `provider.wasm` and `consumer.wasm` are the
real, unmodified componentize-py output.

## The built `.wasm` is not committed

This guest embeds a whole language runtime, so the component is 12–18 MB. Committing
it would put ~90 MB into git history permanently, for a binary anyone can reproduce
with the one command above. Build it locally; the surrounding harnesses pick it up
from this directory.
