---
title: WASI 0.2
description: The 114 WASI 0.2 host functions wazy implements, and how to grant each capability.
---

A binary a stranger produced with `cargo build --target wasm32-wasip2` runs unmodified on the
**114 WASI 0.2 host functions** wazy implements. One call registers them:

```go
import "github.com/samyfodil/wazy/imports/wasip2"

inst, err := component.Instantiate(ctx, r, guestWasm,
	wasip2.With(wasip2.Config{Stdout: os.Stdout})...)
```

`wasip2.With` returns a `[]component.Option`, so spread it into `Instantiate` alongside any of your
own [custom imports](../custom-wit/).

## What is covered

| Interface | |
| --- | --- |
| `wasi:cli` | stdin/stdout/stderr, `get-arguments`, `get-environment`, `initial-cwd`, `exit`, terminal handles |
| `wasi:clocks` | monotonic and wall clocks, `subscribe-duration`, `subscribe-instant` |
| `wasi:filesystem` | preopened directories, open/read/write/seek, metadata, directory iteration, `stat`, `statfs` |
| `wasi:io` | `input-stream` / `output-stream`, `pollable`, batched `poll` |
| `wasi:random` | `get-random-bytes`, insecure variants, seeds |
| `wasi:sockets` | TCP and UDP sockets, `instance-network`, IP name lookup |
| `wasi:http` | outgoing requests, and `incoming-handler` in the other direction |

## Everything is opt-in

`wasip2.Config` is deny-by-default. An empty config gives the guest a component that can compute
and nothing else: no stdio, no files, no clock of consequence, no network.

```go
cfg := wasip2.Config{
	// stdio
	Stdout: os.Stdout,
	Stderr: os.Stderr,
	Stdin:  strings.NewReader(input),

	// process
	Args:       []string{"app", "--flag"},
	Env:        []string{"HOME=/"},
	InitialCWD: "/",

	// filesystem: nothing is visible until you mount it
	FS: wazy.NewFSConfig().
		WithReadOnlyDirMount("./assets", "/assets").
		WithDirMount("./scratch", "/tmp"),

	// network: unregistered until switched on
	AllowTCP: true,
	AllowUDP: false,

	// http
	EnableHTTP: true,
}
```

`Stdin` is read once, up front, and handed to the guest as a fully-available byte string. That is
deliberate: it makes `wasi:io` reads on stdin non-blocking and deterministic.

## Narrowing the network

`AllowTCP` alone gives the guest a real `net.Dial`. To constrain where it can go, supply your own
dialer and resolver — the same hooks the tests use:

```go
cfg.Dialer = func(network, address string) (net.Conn, error) {
	if !allowed(address) {
		return nil, fmt.Errorf("denied: %s", address)
	}
	return net.Dial(network, address)
}

cfg.ResolveIP = func(ctx context.Context, name string) ([]net.IP, error) {
	return resolveInsideOurVPC(ctx, name)
}
```

`Listen` and `ListenPacket` are the inbound equivalents, and `HTTPClient` replaces the
`*http.Client` behind `wasi:http` outgoing requests — set it to something with a timeout and a
restricted transport rather than `http.DefaultClient`.

## HTTP in both directions

**Outgoing.** `EnableHTTP: true` lets the guest make requests through `wasi:http/outgoing-handler`,
using `cfg.HTTPClient` if set.

**Incoming.** A component that exports `wasi:http/incoming-handler` is a request handler. Wrap it:

```go
h := wasip2.Handler(inst)
mux.Handle("/plugin/", http.StripPrefix("/plugin", h))
```

A warm instance answers through `net/http` in 3.37 µs and 45 allocations.

## Clocks

`WallClock` replaces the wall clock with your own function — useful for deterministic tests and for
denying a guest a real timestamp. The monotonic clock always advances.

## WASI 0.1

The preview-1 ABI is a separate package, `imports/wasi_snapshot_preview1`, and applies to core
modules rather than components. See [Core modules and WASI 0.1](../core-modules/).

There is also `imports/wasi_http`, a reimplementation of the **pre-standard** WASI-HTTP ABI from
[wasi-go](https://github.com/stealthrocket/wasi-go), for guests built against that older interface.
The ground truth for which one a guest wants is its import section.
