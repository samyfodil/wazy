# TODOS

## WASI 0.3 / async component-model follow-on
- **What:** Add the native async ABI on top of the real WASI 0.2 Component Model runtime: `future<T>`/`stream<T>`, task/subtask lifecycle, a host event loop, and reworked async interfaces (`wasi:io` is deleted in 0.3, folded into the Canonical ABI).
- **Why:** Would make wazy the only pure-Go runtime that can run async 0.3 components. Go's goroutines+channels back futures/streams more naturally than Wasmtime's Rust event loop — the one place wazy's substrate is an asset.
- **Context:** ~8–10k LOC on top of the p2 runtime. Reference: Wasmtime + `bytecodealliance/wasip3-prototyping`, and the async section of the component-model `definitions.py`. Zero pure-Go prior art. Highest-variance part is async correctness debugging.
- **Depends on / blocked by:** p2 CM runtime shipped and solid (done). Also blocked on the 0.3 spec settling — as of 2026-07 Wasmtime still marks its p3 support experimental/unstable. Do NOT start early; spec churn will waste the work.

## wasi:http — the biggest remaining WASI 0.2 gap
- **What:** The `wasi:http/proxy` world (incoming-handler / outgoing-handler, `wasi:http/types`) is entirely unimplemented. An HTTP-handler component won't run.
- **Why:** For a serverless/functions use case (Taubyte), this is arguably *the* most valuable interface — probably more than any remaining polish. Uses resources (fields, incoming/outgoing request/response, bodies) + streams heavily, so it exercises the machinery already built.
- **Context:** implement `wasi:http/types` (request/response/fields/body resources) + a host handler bridge (Go `net/http` inbound for incoming-handler; Go client for outgoing-handler). Reuse the resource table + stream + list/result marshalling. Verify differentially vs wasmtime (`wasmtime serve`) with a real rustc/`wasi:http/proxy` guest.
- **Depends on / blocked by:** none technical — all prerequisites (resources, streams, poll) are done.

## Minor interface breadth
- **What:** server-side sockets (`tcp.start-listen`/`finish-listen`/`accept`, `udp` bind-and-serve beyond current client/datagram support), `wasi:sockets/ip-name-lookup` (DNS), full `wasi:clocks` (real monotonic/wall-clock where a guest prints time — needs a deterministic/injectable clock to conformance-test), filesystem symlinks/rename.
- **Why:** we implement the interface methods real guests actually call; the rest are fail-loud trap-stubs. Closing these widens the set of guests that run out of the box.
- **Context:** same pattern as the existing sockets/fs work — discover the calls a real guest makes, implement against a Go backing, test behaviorally / vs wasmtime.

## Two different components can't be live on one Runtime at once
- **What:** Both components default their synthesized root core-module name to `wazy:component/core0`, so instantiating a second distinct component on the same `wazy.Runtime` collides ("already instantiated").
- **Why:** fine for one-component-per-runtime (the normal case, and what `CompileCache` reuse targets), but blocks multi-component / multi-tenant reuse of a single Runtime.
- **Context:** derive the synthesized module name from the component identity (bytes hash) instead of a fixed constant. Surfaced during the compile-cache perf work. Also note: `CompileCache`↔`Runtime` pairing is caller-enforced (documented, not checked).
