# TODOS

## WASI 0.3 / async component-model follow-on
- **What:** Add the native async ABI on top of the real WASI 0.2 Component Model runtime: `future<T>`/`stream<T>`, task/subtask lifecycle, a host event loop, and reworked async interfaces (`wasi:io` is deleted in 0.3, folded into the Canonical ABI).
- **Why:** Would make wazy the only pure-Go runtime that can run async 0.3 components. Go's goroutines+channels back futures/streams more naturally than Wasmtime's Rust event loop — the one place wazy's substrate is an asset.
- **Context:** ~8–10k LOC on top of the p2 runtime. Reference: Wasmtime + `bytecodealliance/wasip3-prototyping`, and the async section of the component-model `definitions.py`. Zero pure-Go prior art. Highest-variance part is async correctness debugging.
- **Depends on / blocked by:** p2 CM runtime shipped and solid (done). Also blocked on the 0.3 spec settling — as of 2026-07 Wasmtime still marks its p3 support experimental/unstable. Do NOT start early; spec churn will waste the work.

## Internal nested-component composition — func linking + cross-component resources DONE
- **DONE (func linking, commit cd793ee):** A component binary that declares
  nested component *definitions* (`comp.Instances` + `comp.NestedComponents`),
  instantiates them, and links a sibling's export into another's import runs.
  `wasmtime/fused.wast` passes. A sibling's lifted export is wired into the
  importer as a delegating `hostImport`; an outer export aliasing a nested
  instance's func re-exposes that sub-Instance's boundExport. Scoped via a
  func-alias-to-local-instance reachability check so WASI/shim components are
  untouched. `binary.Component.Bytes` + `Instance.subInstances` added.
- **DONE (cross-component resources, commit e866814):** `resources/multiple-
  resources.wast` passes (run → 42). A resource DEFINED in one nested component
  and IMPORTED + used (created, borrowed, dropped with its destructor) by a
  sibling has one identity across both. Three coupled pieces, all gated behind
  the composition path (`resCanon`/`sharedResources` nil → prior behavior):
  (1) composed sub-instances share ONE handle table, so a handle from the
  definer is directly valid in the importer; (2) a per-component canonicalizer
  (`resCanon` = base + `Component.ResourceDefIndex`, or the sibling's global id
  for an imported resource) maps each component's differing local resource
  indices to one shared-table tag — threaded into the resource canons and
  `resolveHandleArg`; (3) the definer's destructor is resolved at instantiation
  (`Instance.resourceDtors`), registered on the shared table by global id, and
  run by canon `resource.drop` (which previously ran no destructor). A delegating
  import presents resources as opaque u32 (`resourcesToU32FD`) so handles pass
  through untouched. See `composition.go` and [[wazy-wast-conformance]].
- **REMAINING (further wasmtime/resources sub-features):** that suite mixes
  patterns beyond a guest-defined resource: HOST-provided imported-resource
  *constructors* (the embedder supplies a resource impl the guest imports — a
  `[constructor]resource1` trap stub today), component (not instance)
  instantiate-args, and exporting a canon-produced func (`[constructor]t`).
  Each is its own scoped feature; none is an ABI bug.
- **Also deeper fused sub-features** (each skips a `fused.wast` module, logged):
  pass-through shim with empty export names, >16 flat params on an imported func
  (whole-param spilling for a lowered import), func/type instantiate-args,
  self-referential nesting.
- **Acceptance gate:** the `.wast` harness (`wast_conformance_test.go`).

## wasi:http — DONE (both sides), minor breadth remaining
- **Done — full `wasi:http/proxy` world runs.** Both directions verified differentially vs wasmtime:
  - **incoming-handler (server):** a real rustc guest responds to HTTP; vs `wasmtime serve -S cli` (`real_http_incoming.component.wasm`). `(*Instance).ServeHTTP` is a net/http.Handler; enable with `WithWASI(WASIConfig{EnableHTTP: true})`.
  - **outgoing-handler (client):** a real rustc guest makes outbound requests via a Go `http.Client` (`WASIConfig.HTTPClient`); vs `wasmtime serve -S cli -S inherit-network` against a scratch backend (`real_http_outgoing.component.wasm`).
  - Implemented in `wasi_http.go`: the `wasi:http/types` subset a wit-bindgen proxy guest calls (request line read, response write, and the full client path incl. future/incoming-response/incoming-body). Future is synchronous (Do blocks) so subscribe/get are immediate; incoming-body.stream + response body-write both reuse the wasi:io/streams path.
- **Done (incoming request readback):** `incoming-request.headers` + `fields.get` (header read) and `incoming-request.consume` + `incoming-body.stream` (request body), vs `wasmtime serve` (`real_http_request.component.wasm`).
- **Done (outgoing request bodies):** `outgoing-request.body` → the outbound POST body path, vs wasmtime (`real_http_post.component.wasm`).
- **Done (request-options):** `request-options` ctor + `set-connect-timeout`/`set-first-byte-timeout`; `outgoing-handler.handle` applies the timeout as a request deadline (`real_http_reqopts.component.wasm`).
- **Done (trailers, both directions):** response trailers via `outgoing-body.finish(Some(trailers))` (surfaced through net/http's server-side trailer protocol, `real_http_trailers.component.wasm`); request trailers via `incoming-body.finish` → `future-trailers` → `future-trailers.{subscribe,get}` (the nested `option<result<result<option<trailers>,error-code>>>`; plumbed from `r.Trailer`, `real_http_reqtrailers.component.wasm`). `TestRealHTTPTrailers` + `TestRealHTTPRequestTrailers`.
- **Depends on / blocked by:** none technical.

## WASI 0.2 interface breadth — DONE (full compliance)
Every method any off-the-shelf **stable-rust** wasm32-wasip2 guest can call is now implemented; the only fail-loud methods are ones no stable guest can reach (see below). Each closed with a real-guest test in the repo, verified vs wasmtime.
- **Server-side TCP sockets:** `[method]tcp-socket.{start-bind,finish-bind,start-listen,finish-listen,accept,local-address,remote-address,set-listen-backlog-size}`; `WASIConfig.Listen` hook. `TestRealTCPListen` (bind→accept→echo, Go client connects).
- **wasi:clocks:** monotonic-clock (now, resolution, subscribe-duration, subscribe-instant) + wall-clock (now, resolution). Introduced a **shared timer-aware `wasi:io/poll`** (`wasi_poll.go`) replacing the former per-interface no-op block/poll copies — timer pollables genuinely sleep to their deadline (the only thing producing a `std::thread::sleep`'s delay). `WASIConfig.WallClock` injectable. `TestRealClocks`.
- **DNS:** `wasi:sockets/ip-name-lookup` (resolve-addresses, resolve-next-address, subscribe); `WASIConfig.ResolveIP`. `TestRealDNS`.
- **Filesystem:** rename-at (file + dir subtree), create/remove-directory-at (explicit empty-dir `dirs` set), link-at (hard link = content copy). `TestRealFSOps`, `TestRealHardLink`.
- **UDP server:** `[method]udp-socket.local-address` (receive-from-anyone + send-to-sender already worked). `TestRealUDPServer`.
- **wasi:random complete:** get-random-u64, get-insecure-random-bytes/u64, insecure-seed (all crypto/rand). `TestRealRandom`.
- **Socket options:** all tcp/udp setsockopt-style setters (keep-alive, buffer sizes, hop limits) as no-op-Ok (spec permits ignoring these advisory hints) — nothing traps.
- **Capstone:** `TestRealMega` — one guest crossing args/env/random-HashMap/stdin/filesystem/clocks, byte-exact vs wasmtime.
- **Remaining fail-loud (by design):** `wasi:filesystem` symlink-at / readlink-at — symlink CREATION is nightly-only in rust std on wasip2 (`symlink_path` unstable), so no stable-rust guest reaches them. Implement if a non-rust guest ever needs them.

## Per-call realloc closure alloc (deferred — low ROI)
- **What:** `invoke` builds a fresh `abi.Realloc` closure (capturing `ctx`) on every call. It stays on the stack for calls that never touch memory (CallAdd), but escapes to the heap on any string/list parameter (one alloc/call, e.g. CallGreet).
- **Why deferred:** killing it means threading `ctx` through `abi.Realloc` and ~20 store/lower functions in abi (memory.go + flat.go) so the closure can be built once at bind time. That's a wide signature sweep for a single alloc on the string path — poor ratio next to the two wins already taken (lift-iterator pool + top-level-primitive fast path: CallAdd 5→2 allocs, ~245→177 ns/op). Revisit only if string-heavy call profiles demand it.

## DONE: multi-component composition (wasmtime model) + single instantiation path
- A Runtime now hosts any number of component instances (distinct AND multiple of the same), and one component can call another on one Runtime (`TestOneComponentCallsAnotherOnOneRuntime`, `TestTwoLogHelloCoexist`). The graph engine instantiates internals anonymously (`WithName("")`) and wires them via a per-instantiation `experimental.ImportResolver`, so nothing internal touches the global registry -- like wasmtime. Compile cache intact (empty-import rewrite → stable `graphEmptyImportKey`).
- The old `instantiateWithImports` path is DELETED (712 lines incl. its exclusive helpers + ~29 old-only tests); all host-import components now route through the graph engine. Only `instantiateComponent` (trivial single-module, no-import) remains beside it. See the [[wasip2-component-model]] memory for the full mechanism.
