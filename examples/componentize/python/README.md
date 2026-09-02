# `greet` in Python

The `wazy:examples/greeter` world, implemented with
[componentize-py](https://github.com/bytecodealliance/componentize-py). The
component embeds a whole CPython interpreter, so `greet.wasm` is ~18 MB — the
other languages in this set produce kilobytes. That size *is* the lesson: the
host calls all six the same way.

## Install

```sh
python3 -m venv .venv
.venv/bin/pip install componentize-py==0.25.0
```

## Build

```sh
.venv/bin/componentize-py -d greet.wit -w greeter componentize app -o greet.wasm
```

`-w greeter` is the world in `greet.wit`; `app` is the Python module
(`app.py`) implementing it. The module name must differ from the world name —
componentize-py generates a module for the world and Python cannot load two
top-level modules with the same name.

## Toolchain used

| | |
|---|---|
| Python | 3.12.3 |
| componentize-py | 0.25.0 |

## Host requirements

Unlike the compiled-language guests, this component needs two things from the
embedder:

```go
r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfig().
    WithCoreFeatures(api.CoreFeaturesV2|api.CoreFeatureExtendedConst))

inst, err := component.Instantiate(ctx, r, greetWasm, wasip2.With(wasip2.Config{})...)
```

* **`extended-const`** — componentize-py's CPython build initializes a global
  with `i32.add`, which the default `api.CoreFeaturesV2` rejects.
  `api.CoreFeaturesV3` works too.
* **WASI 0.2** — CPython calls `wasi:cli/environment` during interpreter
  start-up, before it ever reaches `greet`. The zero-value `wasip2.Config{}`
  is enough; no stdio or filesystem is needed.

## Verified

```
$ go run .   # host calling greet.wasm
Hello, wazy! (from Python)
```

## The built `.wasm` is not committed

This guest embeds a whole language runtime, so the component is 12–18 MB. Committing
it would put ~90 MB into git history permanently, for a binary anyone can reproduce
with the one command above. Build it locally; the surrounding harnesses pick it up
from this directory.
