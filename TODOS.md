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

## Per-call realloc closure alloc (deferred — low ROI)
- **What:** `invoke` builds a fresh `abi.Realloc` closure (capturing `ctx`) on every call. It stays on the stack for calls that never touch memory (CallAdd), but escapes to the heap on any string/list parameter (one alloc/call, e.g. CallGreet).
- **Why deferred:** killing it means threading `ctx` through `abi.Realloc` and ~20 store/lower functions in abi (memory.go + flat.go) so the closure can be built once at bind time. That's a wide signature sweep for a single alloc on the string path — poor ratio next to the two wins already taken (lift-iterator pool + top-level-primitive fast path: CallAdd 5→2 allocs, ~245→177 ns/op). Revisit only if string-heavy call profiles demand it.

## Two live instances of the *same* component on one Runtime (minor)
- **What:** synthesized names are now namespaced per component (bytes hash, `synthNamePrefix`), so *different* components coexist on one Runtime. But two *live* instances of the *same* component still collide on identical synthesized names.
- **Why:** the documented one-component-at-a-time reuse the `CompileCache` targets; rare in practice. Would need a per-instantiation salt (counter/uuid) on top of the bytes hash, kept compatible with the compile cache. Also note: `CompileCache`↔`Runtime` pairing is caller-enforced (documented, not checked).
