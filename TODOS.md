# TODOS

## WASI 0.3 / async component-model follow-on
- **What:** Add the native async ABI on top of the real WASI 0.2 Component Model runtime: `future<T>`/`stream<T>`, task/subtask lifecycle, a host event loop, and reworked async interfaces (`wasi:io` is deleted in 0.3, folded into the Canonical ABI).
- **Why:** Would make wazy the only pure-Go runtime that can run async 0.3 components. Go's goroutines+channels back futures/streams more naturally than Wasmtime's Rust event loop — the one place wazy's substrate is an asset.
- **Context:** ~8–10k LOC on top of the p2 runtime. Reference: Wasmtime + `bytecodealliance/wasip3-prototyping`, and the async section of the component-model `definitions.py`. Zero pure-Go prior art. Highest-variance part is async correctness debugging.
- **Depends on / blocked by:** p2 CM runtime shipped and solid (done). Also blocked on the 0.3 spec settling — as of 2026-07 Wasmtime still marks its p3 support experimental/unstable. Do NOT start early; spec churn will waste the work.

## Internal nested-component composition — func linking DONE; resource identity remains
- **DONE (func linking, commit cd793ee):** A component binary that declares
  nested component *definitions* (`comp.Instances` + `comp.NestedComponents`),
  instantiates them, and links a sibling's export into another's import now runs.
  `wasmtime/fused.wast` passes (`fused.18` roundtrip across the char boundary
  values). Mechanism: a sibling's lifted export is wired into the importer as a
  delegating `hostImport` (customFD/customResolve from the provider's
  boundExport), so the existing canon-lower/`buildHostWrapper` path lowers calls
  unchanged; an outer export aliasing a nested instance's func re-exposes that
  sub-Instance's boundExport. Scoped to the composition shape via a func-alias-
  to-local-instance reachability check, so WASI/shim components are untouched.
  `binary.Component.Bytes` + `Instance.subInstances` added. See
  [[wazy-wast-conformance]].
- **REMAINING (resource identity across composed components):** gates
  `resources/multiple-resources.wast` and parts of `wasmtime/resources.wast`.
  These now *instantiate and run* through the composition path, then fail on a
  resource handle crossing the fused-adapter boundary. Two coupled problems,
  both traced:
  1. *Within a component*, `resource.new` tags a handle by the resource's
     DEFINITION type index while `own<R>`/`borrow<R>` in a lifted signature use
     the EXPORT-ALIAS index (export-introduces-alias) — the same resource, two
     indices. Fixable with a resource-type canonicalizer on the handleTable
     (`Component.ResourceDefIndex` → tag by the defining index). Prototyped and
     works, but reverted: unexercised by any committed test on its own (it only
     matters together with #2), so it was speculative to ship alone.
  2. *Across the boundary* is the real blocker. The delegating host import gives
     the importer's host wrapper the PROVIDER's `customFD` (the decoder does not
     retain the func signatures inside an imported instance type — the
     documented limitation the composition sidesteps for non-resource funcs).
     So the provider's resource type indices get injected into the importer's
     type space, where they are meaningless, and the importer re-mints handles
     treating a rep as a handle. Cleanly fixing this needs the decoder to
     structurally decode imported-instance-type func signatures so each
     component knows ITS OWN resource indices for imported funcs — then rep-based
     lift_own/lower_own handle transfer at the boundary (the "No cross-instance
     handle transfer" ceiling in `resource.go`). That decoder work is the root
     dependency; it is a large, foundational feature, not a boundary patch.
  3. Even with #1 and #2, `multiple-resources` also needs canon `resource.drop`
     to RUN the resource's destructor (it currently just removes the table
     entry — the "No destructors" ceiling in `resource.go`), because the test
     asserts a `num-live` count that the R1/R2 dtors decrement. wazy runs a dtor
     for a HOST-initiated drop (`DropResource`) but not for a guest's own canon
     `resource.drop`; thread the dtor into `resourceCanonHostFuncGraph`.
  So this one suite is really three coupled ceilings (imported-instance-type
  decode + cross-instance handle transfer + destructor-on-drop) — the full
  resource-composition-and-lifecycle story, a deliberate multi-part project.
- **Also deeper fused sub-features** (each skips a `fused.wast` module, logged):
  pass-through shim with empty export names, >16 flat params on an imported func
  (whole-param spilling for a lowered import), func/type instantiate-args,
  self-referential nesting.
- **Acceptance gate:** the `.wast` harness (`wast_conformance_test.go`) — unskip
  multiple-resources/resources as resource identity lands.
- **Depends on / blocked by:** none technical; it is purely scope.

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
