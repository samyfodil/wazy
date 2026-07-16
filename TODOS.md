# TODOS

## WASI 0.3 / async component-model follow-on
- **What:** Add the native async ABI on top of the real WASI 0.2 Component Model runtime: `future<T>`/`stream<T>`, task/subtask lifecycle, a host event loop, and reworked async interfaces (`wasi:io` is deleted in 0.3, folded into the Canonical ABI).
- **Why:** Would make wazy the only pure-Go runtime that can run async 0.3 components. Go's goroutines+channels back futures/streams more naturally than Wasmtime's Rust event loop — the one place wazy's substrate is an asset.
- **Context:** ~8–10k LOC on top of the p2 runtime. Reference: Wasmtime + `bytecodealliance/wasip3-prototyping`, and the async section of the component-model `definitions.py`. Zero pure-Go prior art. Highest-variance part is async correctness debugging.
- **Depends on / blocked by:** p2 CM runtime shipped and solid (done). Also blocked on the 0.3 spec settling — as of 2026-07 Wasmtime still marks its p3 support experimental/unstable. Do NOT start early; spec churn will waste the work.

## wasi:http — DONE (both sides), minor breadth remaining
- **Done — full `wasi:http/proxy` world runs.** Both directions verified differentially vs wasmtime:
  - **incoming-handler (server):** a real rustc guest responds to HTTP; vs `wasmtime serve -S cli` (`real_http_incoming.component.wasm`). `(*Instance).ServeHTTP` is a net/http.Handler; enable with `WithWASI(WASIConfig{EnableHTTP: true})`.
  - **outgoing-handler (client):** a real rustc guest makes outbound requests via a Go `http.Client` (`WASIConfig.HTTPClient`); vs `wasmtime serve -S cli -S inherit-network` against a scratch backend (`real_http_outgoing.component.wasm`).
  - Implemented in `wasi_http.go`: the `wasi:http/types` subset a wit-bindgen proxy guest calls (request line read, response write, and the full client path incl. future/incoming-response/incoming-body). Future is synchronous (Do blocks) so subscribe/get are immediate; incoming-body.stream + response body-write both reuse the wasi:io/streams path.
- **Done (incoming request readback):** `incoming-request.headers` + `fields.get` (header read) and `incoming-request.consume` + `incoming-body.stream` (request body), vs `wasmtime serve` (`real_http_request.component.wasm`).
- **Done (outgoing request bodies):** `outgoing-request.body` → the outbound POST body path, vs wasmtime (`real_http_post.component.wasm`).
- **Done (request-options):** `request-options` ctor + `set-connect-timeout`/`set-first-byte-timeout`; `outgoing-handler.handle` applies the timeout as a request deadline (`real_http_reqopts.component.wasm`).
- **Remaining — trailers only (niche, fail-loud):** HTTP trailing headers. Response side: `outgoing-body.finish(Some(trailers))` (currently rejects non-nil trailers). Incoming side: `incoming-body.finish` → `future-trailers` → `future-trailers.get`. Rarely used by real proxy guests (mostly grpc-over-h2 status); implement on demand. Everything else a real-world handler needs (both directions, headers read/write, bodies read/write, timeouts) is done.
- **Depends on / blocked by:** none technical.

## Minor interface breadth
- **Done — server-side TCP sockets (listen/accept):** a real rustc `TcpListener::bind`→`accept`→echo guest now listens through wazy's own host; a Go client connects to it and gets its payload back uppercased (`real_tcp_listen.component.wasm`, `TestRealTCPListen`, two varied payloads proving real data flow). Implemented in `wasi_sockets.go`: `[method]tcp-socket.{start-bind,finish-bind,start-listen,finish-listen,accept,local-address,remote-address,set-listen-backlog-size}`. `start-bind` does the real `net.Listen` synchronously (same blocking/single-shot model as `start-connect`); `accept` blocks in `net.Listener.Accept` and mints the `tuple<own<tcp-socket>,own<input-stream>,own<output-stream>>`. New `WASIConfig.Listen` hook (defaults to `net.Listen`; a test injects one to learn the bound ephemeral port), gated by `AllowTCP`. `set-listen-backlog-size` is a documented no-op (OS default backlog).
- **Remaining:** `udp` bind-and-serve beyond current client/datagram support, `wasi:sockets/ip-name-lookup` (DNS), full `wasi:clocks` (real monotonic/wall-clock where a guest prints time — needs a deterministic/injectable clock to conformance-test), filesystem symlinks/rename.
- **Why:** we implement the interface methods real guests actually call; the rest are fail-loud trap-stubs. Closing these widens the set of guests that run out of the box.
- **Context:** same pattern as the existing sockets/fs work — discover the calls a real guest makes, implement against a Go backing, test behaviorally / vs wasmtime.

## Per-call realloc closure alloc (deferred — low ROI)
- **What:** `invoke` builds a fresh `abi.Realloc` closure (capturing `ctx`) on every call. It stays on the stack for calls that never touch memory (CallAdd), but escapes to the heap on any string/list parameter (one alloc/call, e.g. CallGreet).
- **Why deferred:** killing it means threading `ctx` through `abi.Realloc` and ~20 store/lower functions in abi (memory.go + flat.go) so the closure can be built once at bind time. That's a wide signature sweep for a single alloc on the string path — poor ratio next to the two wins already taken (lift-iterator pool + top-level-primitive fast path: CallAdd 5→2 allocs, ~245→177 ns/op). Revisit only if string-heavy call profiles demand it.

## DONE: multi-component composition (wasmtime model) + single instantiation path
- A Runtime now hosts any number of component instances (distinct AND multiple of the same), and one component can call another on one Runtime (`TestOneComponentCallsAnotherOnOneRuntime`, `TestTwoLogHelloCoexist`). The graph engine instantiates internals anonymously (`WithName("")`) and wires them via a per-instantiation `experimental.ImportResolver`, so nothing internal touches the global registry -- like wasmtime. Compile cache intact (empty-import rewrite → stable `graphEmptyImportKey`).
- The old `instantiateWithImports` path is DELETED (712 lines incl. its exclusive helpers + ~29 old-only tests); all host-import components now route through the graph engine. Only `instantiateComponent` (trivial single-module, no-import) remains beside it. See the [[wasip2-component-model]] memory for the full mechanism.
