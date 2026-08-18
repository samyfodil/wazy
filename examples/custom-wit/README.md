# Implementing your own WIT interface, host-side

This example defines a WIT interface of its own and implements it in Go. No
WASI involved.

The interface ([`testdata/store.wit`](testdata/store.wit)) is deliberately not
a toy — it uses the shapes real interfaces use:

```wit
package example:store@1.0.0;

interface kv {
  resource bucket {
    constructor(name: string);
    put: func(key: string, value: list<u8>);
    get: func(key: string) -> option<list<u8>>;
  }
  variant error { no-such-bucket, denied(string) }
  list-keys: func(b: borrow<bucket>) -> result<list<string>, error>;
}
```

A resource with a constructor and methods, an `option` return, a `result`
whose error arm is a `variant`, and lists in both directions.

Run it:

```bash
go run .
```

```
host: created bucket "cache" (rep 1)
guest: hello wazy|missing=true|keys=greeting,target
```

## What to look at

**`TypeTable` builds the signatures.** A WIT type is a graph, not a tree:
`result<list<string>, error>` needs its arms to be nameable from the result,
which is what a type table's `TypeRef`s do. The sugar (`List`, `Option`,
`Result`, `Variant`, `Own`, `Borrow`, `Func`) means you never write a
`TypeRef` yourself.

**`WithImportCustom` registers them.** The simpler `WithImport` takes a flat
list of types and cannot express a composite whose children are composites,
which is most of this interface.

**`WithResourceTag` is not optional.** Any interface with a resource needs it.
The guest drops its owned handle through a canon carrying the component
binary's own type index, while the host mints handles under a tag it chose;
this maps one to the other. Leave it out and everything works until the first
drop, which then fails with a cross-type error that doesn't mention tags.

**Values have shapes.** A `record` and a `tuple` arrive as `[]Value` in
declaration order; a `variant` is a `VariantValue{Disc, Payload}`; a `result`
is a `ResultValue{IsErr, Payload}`; `option` is `nil` for none or the inner
value for some.

A `list` of a fixed-width primitive arrives as the Go slice of that primitive —
`[]byte` for `list<u8>`, `[]uint32` for `list<u32>`, and so on — while a list of
anything else, `list<string>` included, arrives as `[]Value`. `component.ListOf`
reads either:

```go
value, err := component.ListOf[byte](args[2])    // list<u8>
keys, err := component.ListOf[string](args[0])   // list<string>
```

It returns the typed shape as it arrived rather than copying, so reaching for it
costs nothing when the list is already typed. Both shapes are accepted when you
*return* a list, so a host func producing one can hand over whichever it has.

Note that returning a Go `error` from a host func *traps the guest* — a
WIT-declared failure is a `ResultValue` with `IsErr` set, which is an ordinary
value the guest handles.

## Testing host funcs without a guest

Compiling a component per error branch is impractical.
[`component/componenttest`](../../component/componenttest) applies the same
Options and hands back the registered funcs so they can be called directly.
It does not exercise the Canonical ABI, so keep at least one real-guest test
per interface — like this example — and use the harness for the branches
underneath it.

## Rebuilding the guest

`testdata/store.wasm` is a Rust component built against the same `.wit`:

```bash
cargo build --release --target wasm32-wasip2
```

with `wit-bindgen` generating the bindings from `wit/store.wit`.
