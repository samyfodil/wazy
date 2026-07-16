# TODOS

## WASI 0.3 / async component-model follow-on
- **What:** Add the native async ABI on top of the real WASI 0.2 Component Model runtime: `future<T>`/`stream<T>`, task/subtask lifecycle, a host event loop, and reworked async interfaces (`wasi:io` is deleted in 0.3, folded into the Canonical ABI).
- **Why:** Would make wazy the only pure-Go runtime that can run async 0.3 components. Go's goroutines+channels back futures/streams more naturally than Wasmtime's Rust event loop — the one place wazy's substrate is an asset.
- **Context:** ~8–10k LOC on top of the p2 runtime. Reference: Wasmtime + `bytecodealliance/wasip3-prototyping`, and the async section of the component-model `definitions.py`. Zero pure-Go prior art. Highest-variance part is async correctness debugging.
- **Depends on / blocked by:** p2 CM runtime shipped and solid (done). Also blocked on the 0.3 spec settling — as of 2026-07 Wasmtime still marks its p3 support experimental/unstable. Do NOT start early; spec churn will waste the work.

## wasi:http — server side DONE, client side remaining
- **Done (incoming-handler):** a real rustc `wasi:http/incoming-handler` guest runs on wazy and responds to HTTP requests, verified byte-identical vs `wasmtime serve -S cli` (`wasi_http.go`, `real_http_test.go`, fixture `real_http_incoming.component.wasm`). `(*Instance).ServeHTTP` is a net/http.Handler; enable with `WithWASI(WASIConfig{EnableHTTP: true})`. Implemented `wasi:http/types`: incoming-request.{method, path-with-query}, fields ctor+set, outgoing-response ctor+set-status-code+body, outgoing-body.{write, finish}, response-outparam.set (+ full error-code variant). Body writes reuse the wasi:io/streams output-stream path.
- **Remaining (outgoing-handler, client side):** `wasi:http/outgoing-handler.handle` + `outgoing-request`, `request-options`, `future-incoming-response`, `incoming-response`, `incoming-body` — bridge to a Go `http.Client`. Also on the incoming side, not yet: request header readback, incoming-request.consume (request body), response trailers.
- **Context:** same differential discipline — build a real rustc guest that makes an outbound request (verify vs `wasmtime serve` against a scratch backend), implement the client resources against `net/http`, reuse the resource table + streams (incoming-body's input-stream can reuse fsStreamNode like stdin does). Poll/future may be needed for `future-incoming-response.get`.
- **Depends on / blocked by:** none technical.

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
