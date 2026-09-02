# `wazy:compose/greeter` in JavaScript

Both halves of the cross-language composition matrix, built with
`jco componentize`:

| file | world | role |
| --- | --- | --- |
| `provider.wasm` | `provider` | **exports** `wazy:compose/greeter@0.1.0` |
| `consumer.wasm` | `consumer` | **imports** `wazy:compose/greeter@0.1.0`, exports `run` |

`greeter.wit` is the shared contract, byte-identical across every language in
the matrix.

## Install

```bash
npm i -g @bytecodealliance/jco @bytecodealliance/componentize-js @bytecodealliance/preview2-shim
```

`preview2-shim` is not optional with a **global** install: `componentize-js`
resolves it as a sibling, and `npm i -g` of the first two alone leaves it
missing. (A local `npm i` gets it transitively, but then `node_modules` lands
next to your source — which is why it is installed globally here and no
`node_modules` exists in this directory.)

## Build

```bash
jco componentize provider.js --wit greeter.wit --world-name provider -o provider.wasm
jco componentize consumer.js --wit greeter.wit --world-name consumer -o consumer.wasm
```

`greeter.wit` holds two worlds, so `--world-name` is required; without it jco
cannot pick one.

## Toolchain used

| tool | version |
| --- | --- |
| node | v23.11.1 |
| npm | 10.9.2 |
| `@bytecodealliance/jco` | 1.32.1 |
| `@bytecodealliance/componentize-js` | 0.22.0 |
| `@bytecodealliance/preview2-shim` | 0.23.0 |
| `wasm-tools` | 1.253.0 |
| `wac-cli` | 0.10.1 |
| Go (host) | 1.26.0 linux/amd64 |

## How the WIT maps to JavaScript

**Exporting an interface** (`provider`). `export greeter;` exports a whole
interface, not a bare function, so the module exports **one object** named
after the interface, with the interface's functions as its methods. jco maps
WIT kebab-case to lowerCamelCase, so `greet-all` becomes `greetAll`. The
`visitor` record arrives as a plain object with `name` and `id` properties.

```js
export const greeter = {
  greet(who) { ... },
  greetAll(names) { ... },
};
```

**Exporting a top-level func** (`consumer`). `export run: func(...)` is not
inside an interface, so it is a bare named export: `export function run()`.

**Importing an interface** (`consumer`). componentize-js turns a WIT import
into a JS **module import**, and the specifier is the fully qualified
interface name **including the version**:

```js
import { greet, greetAll } from 'wazy:compose/greeter@0.1.0';
```

Drop the `@0.1.0` and the specifier does not resolve. Nothing has to be
installed for that import — it is satisfied by the component's import section,
not by npm.

**`list<string>` in both directions** is a plain `Array<string>`, and an empty
array round-trips as an empty array: `greetAll` is `names.map(...)`, with no
special case for length 0.

## Verification

```
$ wasm-tools validate --features all provider.wasm && echo OK
OK
$ wasm-tools validate --features all consumer.wasm && echo OK
OK
```

```
$ wasm-tools component wit provider.wasm     # (WASI imports elided)
world root {
  import wasi:io/error@0.2.10;
  ...
  export wazy:compose/greeter@0.1.0;
}

$ wasm-tools component wit consumer.wasm     # (WASI imports elided)
world root {
  import wazy:compose/greeter@0.1.0;
  import wasi:io/error@0.2.10;
  ...
  export run: func() -> list<string>;
}
```

Both halves were run on wazy **individually**, and both are correct:

```
### PROVIDER HALF ###  (called directly through CallExport)
greet({name:"wazy", id:42})    = "Hello, wazy #42! (from JavaScript)"
greet-all(["a", "b"])          = ["a (via JavaScript)" "b (via JavaScript)"]
greet-all([])                  = [] (len=0)

### CONSUMER HALF ###  (greeter supplied by a Go host func, not by JS)
run()[0] = "Hello, wazy #42! (from GoHost)"
run()[1] = "a (via GoHost)"
run()[2] = "empty-len=0"
```

The consumer run above is the proof that it hardcodes nothing: the greeter was
a Go host function saying `GoHost`, and `GoHost` is what came out.

## Running it

These components embed a whole JavaScript engine (StarlingMonkey/SpiderMonkey),
which reaches for `wasi:clocks`, `wasi:random` and stdio while initialising, so
the host must pass WASI 0.2:

```go
r := wazy.NewRuntime(ctx)
inst, err := component.Instantiate(ctx, r, wasm, wasip2.With(wasip2.Config{})...)
```

`jco componentize ... -d all` builds a variant that imports **nothing at all**
(verified: both pure halves run on wazy with no `wasip2` options), at the cost
of `console.log` and of readable errors — a JS exception then traps with no
message. The artifacts committed here are the normal builds, so errors stay
legible.

## Known gap: the composed component does not instantiate on wazy

`self.wasm` could not be produced as a working smoke test. Composition itself
succeeds and the result validates, but **wazy cannot instantiate it**:

```
$ wasm-tools compose consumer.wasm -d provider.wasm -o self.wasm
composed component `self.wasm`
$ wasm-tools validate --features all self.wasm && echo VALID
VALID
$ # run self.wasm on wazy
panic: instantiate: component/instance: component instance 18 arg
"wasi:io/error@0.2.10" references instance 0, which is not a prior nested
instantiation
```

This is **not** specific to JavaScript, to WASI, or to `wasm-tools compose`.
All four combinations fail the same way:

| composer | halves | error |
| --- | --- | --- |
| `wasm-tools compose` | normal | `instance 18 arg "wasi:io/error@0.2.10" references instance 0` |
| `wasm-tools compose` | `-d all`, zero imports | `instance 2 arg "wazy:compose/greeter@0.1.0" references instance 1` |
| `wac plug` | normal | `instance 18 arg "wasi:io/error@0.2.10" references instance 0` |
| `wac plug` | `-d all`, zero imports | `instance 2 arg "wazy:compose/greeter@0.1.0" references instance 1` |

### What wazy is rejecting

A composed component links its two halves like this (the `-d all` build, which
has no WASI at all — this is the whole component):

```wat
(instance (;0;) (instantiate 1))                                   ;; the provider
(alias export 0 "wazy:compose/greeter@0.1.0" (instance (;1;)))     ;; <-- the step that fails
(instance (;2;) (instantiate 0
    (with "wazy:compose/greeter@0.1.0" (instance 1))               ;; the consumer
  )
)
```

The provider *exports an interface*, so the thing the consumer needs is not the
provider instance itself but the sub-instance the provider exports — reached by
`alias export`. That alias occupies a slot in the component-instance index
space, and the consumer's instantiate-arg names that slot.

`instantiateNestedInstances` in `internal/component/instance/graph.go` resolves
an instance-sort instantiate-arg only against `byIdx`, which is keyed by the
slots of *nested instantiations* (`comp.Instances`, `Kind == 0x00`). An
alias-of-export slot is not in `comp.Instances`, so the lookup misses and
instantiation fails (graph.go:882). In the WASI build the very same lookup
misses one step earlier, on an **imported** instance slot, which is likewise
not a nested instantiation.

Both are legal: the component model lets an instantiate-arg of instance sort
name any index in the component-instance index space — imported instances and
aliases included, not just prior nested instantiations.

### 38-line reproduction

No JavaScript, no WASI, no 12 MB engine:

```wat
(component
  (component $P
    (core module $m
      (func (export "f") (result i32) i32.const 42)
    )
    (core instance $mi (instantiate $m))
    (func $f (result u32) (canon lift (core func $mi "f")))
    (instance $iface (export "f" (func $f)))
    (export "p:q/i" (instance $iface))
  )

  (component $C
    (import "p:q/i" (instance $i (export "f" (func (result u32)))))
    (alias export $i "f" (func $if))
    (core func $cf (canon lower (func $if)))
    (core module $m2
      (import "i" "f" (func $imp (result i32)))
      (func (export "run") (result i32) call $imp)
    )
    (core instance $mi2 (instantiate $m2
      (with "i" (instance (export "f" (func $cf))))
    ))
    (func $run (result u32) (canon lift (core func $mi2 "run")))
    (export "run" (func $run))
  )

  (instance $pi (instantiate $P))
  (alias export $pi "p:q/i" (instance $ii))
  (instance $ci (instantiate $C (with "p:q/i" (instance $ii))))
  (alias export $ci "run" (func $r))
  (export "run" (func $r))
)
```

```
$ wasm-tools validate --features all repro.wasm && echo VALID
VALID
$ # instantiate on wazy
INSTANTIATE ERROR: component/instance: component instance 2 arg "p:q/i"
references instance 1, which is not a prior nested instantiation
```

Change only the two lines that need the alias — have `$P` export `f` at its
root so the arg can name the nested instantiation directly —

```wat
    (export "f" (func $f))                                    ;; instead of the instance export
    ...
  (instance $ci (instantiate $C (with "p:q/i" (instance $pi))))   ;; no alias
```

— and the same component runs:

```
run() = [42]
```

So the trigger is exactly the alias-of-export (or imported-instance) slot as an
instance-sort instantiate-arg.

This is the general form of the `--backend quickjs` gap already noted in
`examples/componentize/javascript/README.md` (`instance 20 arg
"local:init/module-loader" references instance 19`). That was filed as a
QuickJS-backend quirk; it is in fact the mainline composition path, and it
blocks every cross-language pair in this matrix, in every language, regardless
of composer.

## The built `.wasm` is not committed

This guest embeds a whole language runtime, so the component is 12–18 MB. Committing
it would put ~90 MB into git history permanently, for a binary anyone can reproduce
with the one command above. Build it locally; the surrounding harnesses pick it up
from this directory.
