# compose — Rust (provider + consumer)

Both halves of the cross-language composition matrix, in Rust, built with
`cargo component`. The contract is [`greeter.wit`](greeter.wit) — byte-identical
in every language of the matrix.

```wit
package wazy:compose@0.1.0;

interface greeter {
  record visitor { name: string, id: u32 }
  greet: func(who: visitor) -> string;
  greet-all: func(names: list<string>) -> list<string>;
}

world provider { export greeter; }
world consumer { import greeter; export run: func() -> list<string>; }
```

| artifact | world | source |
| --- | --- | --- |
| `provider.wasm` (49 837 B) | `provider` | [`provider/src/lib.rs`](provider/src/lib.rs) |
| `consumer.wasm` (49 630 B) | `consumer` | [`consumer/src/lib.rs`](consumer/src/lib.rs) |

`provider` answers `Hello, <name> #<id>! (from Rust)` and `<name> (via Rust)`.
`consumer` hardcodes none of that: all three strings it returns crossed the
interface, so a bad lift or lower shows up in the output rather than being
masked by a local constant.

## Toolchain

| tool | version |
| --- | --- |
| rustc | 1.90.0 (1159e78c4 2025-09-14) |
| cargo | 1.90.0 |
| cargo-component | 0.21.1 |
| wasm-tools | 1.253.0 |
| target | `wasm32-wasip2` |

```sh
rustup target add wasm32-wasip2
cargo install cargo-component --locked
```

## Build

```sh
cargo component build --release --target wasm32-wasip2
cp target/wasm32-wasip2/release/provider.wasm provider.wasm
cp target/wasm32-wasip2/release/consumer.wasm consumer.wasm
```

`cargo component` regenerates `*/src/bindings.rs` from `greeter.wit` on every
build, so the generated bindings are not committed.

> cargo-component 0.21.1 also builds its default `wasm32-wasip1` target on the
> side, and its closing lines read `Creating component
> target/wasm32-wasip1/release/…`. Ignore them — the components you want are the
> `wasm32-wasip2` ones named in the `cp` above.

## Verify

```sh
wasm-tools validate --features all provider.wasm    # passes
wasm-tools validate --features all consumer.wasm    # passes
wasm-tools component wit provider.wasm | grep export   # export wazy:compose/greeter@0.1.0;
wasm-tools component wit consumer.wasm | grep -E 'import wazy|export run'
```

`provider.wasm` exports `wazy:compose/greeter@0.1.0`; `consumer.wasm` imports
`wazy:compose/greeter@0.1.0` and exports `run: func() -> list<string>`.

Neither `greet` nor `run` touches WASI, but Rust's `std` links its panic and
stdio machinery unconditionally, so both components additionally *declare*
`wasi:cli`, `wasi:io`, `wasi:clocks` and `wasi:filesystem` imports. Pass
`wasip2.With(wasip2.Config{})` when instantiating.

## Status on wazy

Verified against wazy at commit `31dcb15d` (a pristine `git archive HEAD` tree,
so none of the repo's uncommitted working-tree changes are involved).

Each half was run against wazy on its own and is **fully correct** — the
provider's exports called directly from Go, and the consumer's `run` against a
Go host implementation of `greeter`:

```
== provider.wasm exports, called directly from Go ==
  greet({wazy,42})   = "Hello, wazy #42! (from Rust)"
  greet-all([a,b])   = ["a (via Rust)" "b (via Rust)"] (len 2)
  greet-all([])      = [] (len 0)

== consumer.wasm run(), greeter implemented by the Go host ==
  run() returned 3 strings:
    [0] "Hello, wazy #42! (from HostGo)"
    [1] "a (via HostGo)"
    [2] "empty-len=0"
```

Both directions of `record { string, u32 }` and of `list<string>`, including the
empty list, are exercised there and all are correct.

**The composed component does not yet run on wazy.** Composing the two and
instantiating the result fails:

```
$ wasm-tools compose consumer.wasm -d provider.wasm -o self.wasm
composed component `self.wasm`
$ # instantiate self.wasm on wazy
instantiate: component/instance: component instance 10 arg
"wasi:cli/environment@0.2.3" references instance 0, which is not a prior
nested instantiation
```

This is a wazy limitation, not a defect in these artifacts — see below. Nothing
here works around it.

### The wazy gap: instance args that are not nested instantiations

`internal/component/instance/graph.go` keys its sibling map `byIdx` only by
*instance definitions* (`ComponentInstanceFromDefinition`). When an
`instantiate` argument names a component instance produced any other way, the
lookup misses and instantiation fails. Two producers are affected, and a
composed component uses both:

1. **`ComponentInstanceFromAlias`** — `(alias export $p "iface" (instance))`.
   This is the fundamental blocker: *every* composition links its consumer to
   its provider through exactly this alias, so no composed pair can avoid it.
2. **`ComponentInstanceFromImport`** — an imported instance forwarded to a
   child, which is how a composer shares host WASI between both halves.

`internal/component/binary/componentinstancespace.go` already models all four
producers, and its `ResolveComponentInstance` doc notes it returns `ok == false`
for "an alias of another instance's instance-typed export" — precisely case 1.
The gap is in graph linking, not in decoding.

Both official composers emit this shape, so neither is a way around it:
`wasm-tools compose` 1.253.0 (deprecated) and `wac plug` 0.10.1 produce the same
edge and fail identically. No component in
`internal/component/instance/testdata/` contains an
`(alias export N "…" (instance …))` used as an instantiate arg, so this linking
edge appears to be untested.

Minimal language-independent reproducer — `run()` should return 7:

```wat
(component
  (component $P
    (core module $m (func (export "f") (result i32) i32.const 7))
    (core instance $mi (instantiate $m))
    (func $f (result u32) (canon lift (core func $mi "f")))
    (instance $pi (export "f" (func $f)))
    (export "test:p/i" (instance $pi))
  )
  (component $C
    (import "test:p/i" (instance $ii (export "f" (func (result u32)))))
    (alias export $ii "f" (func $af))
    (core func $cf (canon lower (func $af)))
    (core module $m
      (import "i" "f" (func $imp (result i32)))
      (func (export "run") (result i32) call $imp)
    )
    (core instance $mi (instantiate $m (with "i" (instance (export "f" (func $cf))))))
    (func $run (result u32) (canon lift (core func $mi "run")))
    (export "run" (func $run))
  )
  (instance $p (instantiate $P))
  (alias export $p "test:p/i" (instance $pii))   ;; <-- the edge under test
  (instance $c (instantiate $C (with "test:p/i" (instance $pii))))
  (alias export $c "run" (func $r))
  (export "run" (func $r))
)
```

It validates (`wasm-tools validate --features all`) and fails on wazy with
`component instance 2 arg "test:p/i" references instance 1, which is not a
prior nested instantiation`. Changing only that one edge — exporting `f` from
`$P` directly and passing `(with "test:p/i" (instance $p))`, a real nested
instantiation — makes the identical component return `run() = 7`. The alias is
the whole difference.

## Composing against another language

Once the gap above is closed, either half pairs with any other language's:

```sh
wasm-tools compose consumer.wasm -d ../<lang>/provider.wasm -o pair.wasm
wasm-tools compose ../<lang>/consumer.wasm -d provider.wasm -o pair.wasm
```

The provider's language is visible in the output text, so a composed `run()`
names which side actually executed.
