---
title: Custom WIT interfaces
description: Declare a plugin contract in a .wit file and implement the host half in ordinary Go.
---

WASI is not the only interface a component can import. Declare your own in a `.wit` file, implement
the host half in Go, and any language that compiles to Wasm can implement the other half.

## The contract

```wit title="store.wit"
package example:store@1.0.0;

interface kv {
  /// A named bucket of key/value pairs, owned by the host.
  resource bucket {
    constructor(name: string);
    put: func(key: string, value: list<u8>);
    get: func(key: string) -> option<list<u8>>;
  }

  variant error {
    no-such-bucket,
    denied(string),
  }

  /// Returns every key in the bucket, or why it could not.
  list-keys: func(b: borrow<bucket>) -> result<list<string>, error>;
}

world app {
  import kv;
  export run: func() -> string;
}
```

That is [`examples/custom-wit`](https://github.com/samyfodil/wazy/tree/main/examples/custom-wit),
which is worth reading end to end: it covers resources, a `borrow`, a `variant` and a
`result<list<string>, variant>`.

## Implementing the host half

`list-keys` returns `result<list<string>, error>` — both arms are composites, so neither can be
spelled inline. Composite types live in a **`TypeTable`**: one table per function signature, which
hands back a `TypeRef` for each type it interns, so an outer type can name an inner one by index.
Build the table, build the `FuncDesc` from it, and register the pair with `WithImportCustom`:

```go
// The interface name as the guest imports it. Matching ignores the "@x.y.z"
// suffix, so one registration serves every patch version.
const iface = "example:store/kv@1.0.0"

// The tag this host mints bucket handles under (see Resources, below).
const bucketTag uint32 = 1

// The host state behind the resource. A "rep" is the host-side index; the
// engine mints the guest's handle over it.
var (
	buckets = map[uint32]map[string][]byte{} // rep -> contents
	nextRep uint32
)

// list-keys(b: borrow<bucket>) -> result<list<string>, error>
keysTbl := component.NewTypeTable()
errRef := keysTbl.Variant(
	component.VariantCaseSpec{Name: "no-such-bucket"},
	component.VariantCaseSpec{Name: "denied", Type: component.Prim("string")},
)
keysFD := keysTbl.Func(
	[]component.TypeRef{keysTbl.Borrow(bucketTag)},
	keysTbl.Result(keysTbl.List(component.Prim("string")), errRef),
)

listKeys := func(_ context.Context, args []component.Value) ([]component.Value, error) {
	// A borrow arrives already resolved to the host rep you handed out.
	b, ok := buckets[args[0].(uint32)]
	if !ok {
		// The err arm, carrying the payload-less first variant case.
		return []component.Value{component.ResultValue{
			IsErr:   true,
			Payload: component.VariantValue{Disc: 0},
		}}, nil
	}
	keys := make([]string, 0, len(b))
	for k := range b {
		keys = append(keys, k)
	}
	sort.Strings(keys) // map order is not a contract; the guest prints these
	// list<string> is not a list of a fixed-width primitive, so it lowers as
	// []component.Value.
	out := make([]component.Value, len(keys))
	for i, k := range keys {
		out[i] = k
	}
	return []component.Value{component.ResultValue{Payload: out}}, nil
}

opts := []component.Option{
	component.WithImportCustom(iface, "list-keys", listKeys, keysFD, keysTbl.Resolver()),
}

inst, err := component.Instantiate(ctx, r, guestWasm, opts...)
```

A `FuncDesc` and the table it was built from must be passed together — the `Resolver` is how the
engine walks a `TypeRef` back to a descriptor. The table is not safe for concurrent use: build the
signature on one goroutine, then treat it as read-only.

:::danger
`return nil, err` from a host function does **not** produce the `err` arm of a `result`. Returning
a Go `error` **traps the guest call**. A WIT-declared failure is an ordinary value —
`component.ResultValue{IsErr: true, Payload: …}`, returned with a `nil` error, as above. Keep the
Go `error` return for "this call cannot proceed at all".
:::

### When `WithImport` is enough

`component.WithImport(iface, name, fn, params, results []component.TypeDesc)` takes flat lists of
top-level descriptors and no table, so it can express any signature whose types have no children to
name — primitives, and bare handles:

```go
// [constructor]bucket(name: string) -> own<bucket>
ctor := func(_ context.Context, args []component.Value) ([]component.Value, error) {
	nextRep++
	buckets[nextRep] = map[string][]byte{}
	return []component.Value{nextRep}, nil // a top-level own<R>: return the rep
}

component.WithImport(iface, "[constructor]bucket", ctor,
	[]component.TypeDesc{component.PrimitiveDesc{Prim: "string"}},
	[]component.TypeDesc{component.OwnDesc{ResourceType: bucketTag}},
)
```

The moment a type has a child — a `list<u8>` parameter, an `option<T>` return, either arm of a
`result` — it needs a table slot, and the registration becomes `WithImportCustom`.
[`examples/custom-wit`](https://github.com/samyfodil/wazy/blob/main/examples/custom-wit/custom_wit.go)
builds a table for all four of the interface's functions, which is the simpler habit: one shape for
every registration.

:::note
Declared record field **names** never reach the wire. The Canonical ABI transmits field order and
types only, so renaming a field in your `TypeTable` changes nothing observable — a test that
mutates a name and still passes has proven nothing. Test with fields of *different* types, or with
the order changed.
:::

## Resources on the host side

The `bucket` resource above is host-owned. Give it a tag, and give the tag a destructor:

```go
opts = append(opts,
	component.WithResourceTag(iface, "bucket", bucketTag),
	component.WithHostResourceDtor(bucketTag, func(ctx context.Context, rep uint32) error {
		delete(buckets, rep)
		return nil
	}),
)
```

`rep` is the host-side representation you handed out when the resource was created — an index into
your own table, typically. When the guest drops the handle, or the instance closes, wazy calls the
destructor exactly once.

Registering a tag without a destructor leaks. If two interfaces share a tag, compose their cleanup
into a single function — the last registration for a tag wins.

## Async imports

An import the host cannot answer immediately is registered with `WithAsyncImport`. The guest awaits
it; your Go code resolves it whenever the answer arrives, possibly from another goroutine:

```go
component.WithAsyncImport(iface, "fetch", fetchAsync, params, results)
```

See [Async](../async/) for the task model this plugs into.

## Building the guest

Any toolchain that emits a component works. From Rust:

```bash
cargo component build --release        # or: cargo build --target wasm32-wasip2
```

From Go, TinyGo, C or Zig, `wit-bindgen` generates the guest bindings and `wasm-tools component new`
wraps the core module. The
[examples directory](https://github.com/samyfodil/wazy/tree/main/examples) has the exact commands
per toolchain, and commits the resulting `.wasm` beside the source.
