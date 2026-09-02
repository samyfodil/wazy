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

Each imported function is registered with its interface name, its function name, the Go
implementation, and the parameter and result types:

```go
opts := []component.Option{
	component.WithImport("example:store/kv", "list-keys",
		func(ctx context.Context, args []component.Value) ([]component.Value, error) {
			b := args[0]
			keys, err := listKeys(b)
			if err != nil {
				return nil, err   // lifts to the `error` variant
			}
			return []component.Value{keys}, nil
		},
		[]component.TypeDesc{bucketBorrow},
		[]component.TypeDesc{resultOfListStringOrError},
	),
}

inst, err := component.Instantiate(ctx, r, guestWasm, opts...)
```

`TypeDesc` describes the WIT type on each side so the Canonical ABI knows how to lift and lower it.
Returning a Go `error` produces the `err` arm of a `result`.

:::note
Declared record field **names** never reach the wire. The Canonical ABI transmits field order and
types only, so renaming a field in your `TypeTable` changes nothing observable — a test that
mutates a name and still passes has proven nothing. Test with fields of *different* types, or with
the order changed.
:::

## Resources on the host side

The `bucket` resource above is host-owned. Give it a tag, and give the tag a destructor:

```go
const bucketTag = 1

opts = append(opts,
	component.WithResourceTag("example:store/kv", "bucket", bucketTag),
	component.WithHostResourceDtor(bucketTag, func(ctx context.Context, rep uint32) error {
		return buckets.close(rep)
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
component.WithAsyncImport("example:store/kv", "fetch", fetchAsync, params, results)
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
