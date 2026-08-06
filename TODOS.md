# TODOS

## Component Model async ABI — DONE
- **DONE (the async ABI on top of the WASI 0.2 CM runtime):** callback and stackful lift, `future<T>`/`stream<T>` (rendezvous copy, per-element `own<R>` transfer, sync + async read/write, cancellation), task/subtask lifecycle (cancellation, backpressure, context storage, borrow scopes), a deterministic per-composition scheduler, and a `thread.*` cooperative fiber runtime (`new-indirect`/`yield`/`suspend`/`yield-then-resume`). Goroutines + an unbuffered-channel baton give exactly-one-runnable semantics (race-free, verified under `-race`) — the substrate advantage the plan predicted.
- **Conformance:** all 31 official Component Model async `.wast` suites pass (0 skip, 0 fail), cross-checked by a differential trace-oracle vs the spec reference (`definitions.py`). One suite (`sync-streams`) carries a fixture fix filed upstream as WebAssembly/component-model#679 — the runtime itself needed no change. Design docs: `docs/component-model-async-*.md`.
- **Follow-on still open:** the full WASI 0.3 host-interface surface (the reworked `wasi:io`/`wasi:sockets`/`wasi:http` 0.3 worlds folded into the async ABI) once the 0.3 spec settles — Wasmtime still marks its p3 support experimental as of 2026-07. The async ABI substrate is ready for it.

## Internal nested-component composition — func linking + cross-component resources DONE
- **DONE (func linking, commit cd793ee):** A component binary that declares
  nested component *definitions* (`comp.Instances` + `comp.NestedComponents`),
  instantiates them, and links a sibling's export into another's import runs.
  `wasmtime/fused.wast` passes. A sibling's lifted export is wired into the
  importer as a delegating `hostImport`; an outer export aliasing a nested
  instance's func re-exposes that sub-Instance's boundExport. Scoped via a
  func-alias-to-local-instance reachability check so WASI/shim components are
  untouched. `binary.Component.Bytes` + `Instance.subInstances` added.
- **DONE (cross-component resources, commits e866814 + 82ce821):** `resources/
  multiple-resources.wast` passes (run → 42). A resource DEFINED in one nested
  component and IMPORTED + used (created, borrowed, dropped with its destructor)
  by a sibling works, with the spec-correct per-instance model:
  - Each sub-instance has its OWN handle table (two instances of one component
    number handles independently — wasmtime/resources.wast res.16). A resource
    CROSSING a delegating import is transferred by REP (lift_own/lower_own): the
    importer wrapper reduces its handle to the rep, the shim re-mints in the
    provider's table for the provider call (`repToProviderHandle`), provider
    results reduce back to reps (`providerHandleToRep`).
  - The delegating import re-points the provider's signature to the IMPORTER's
    own resource type indices (`translateResourceFD` synthesizes own<importerIdx>
    / borrow<importerIdx> + a resolver), so the importer mints under its own
    index — consistent with its resource.drop.
  - `Component.ResourceDefIndex` reduces a resource's deftype/export-alias
    indices to one tag (`resCanon`). The definer's dtor is resolved at
    instantiation (`Instance.resourceDtors`) and registered on the IMPORTER's
    table under the tag its resource.drop uses (`cfg.importedResDtors`); canon
    resource.drop runs it (previously ran none).
  - handleTable gained the reference Table's free list (dropped index reused
    before the counter grows). See `composition.go` + [[wazy-wast-conformance]].
- **DONE (guest own-resource destructors, commit ba62606):** canon
  `resource.drop` now runs a component's OWN destructor (previously only a
  host-initiated `DropResource` ran one). Registered before core modules
  instantiate (a `start` may drop mid-graph) via a lazy resolver
  (`handleTable.dtors` is `func() api.Function`). Proven by
  `TestRealOwnResourceDtorOnDrop` (wasmtime/resources.wast module 20: free list +
  own-dtor drop-counting). res.3/16/18/20 of that suite now run correctly.
- **DONE (host-provided resources + borrow-lend, commit 28bcdde):**
  wasmtime/resources.wast now passes -- the 7th and last official suite the
  harness runs, so all 7 pass. A guest dropping an own<R> of a HOST-provided
  resource runs a Go destructor (`withHostResourceDtor` / `cfg.hostResDtors`);
  `handleTable.dtors` is now `func(ctx, rep) error` so guest core-func dtors
  (lazy) and host Go dtors share one path. Borrow-lend: lifting a borrow<T>
  host-call arg lends the resource for the call's duration (released after via
  `liftHostArgs`'s returned lends), so an own<T> take of the same resource traps
  "cannot remove owned resource while borrowed". The harness supplies the test's
  `host` resource1 (constructor/assert/drops/last-drop/methods/take-own), the
  test-runner plumbing wasmtime provides.
- **DONE (zero skips, commits 679cf3a + 1483b08 + beeeba3):** every vendored
  module in all 7 official suites now runs -- no skips.
  - types.1: decoder parses core:type definitions (func/module types) inside an
    instance/component type (679cf3a).
  - res.25: export a canon-produced func (a `[constructor]t` = lift(resource.new))
    by building its canon host module on demand and wrapping it in a passthrough
    shim; plus the own<T>-arg convention fix (own is a handle, only borrow of a
    receiver-defined resource is a rep) (1483b08).
  - res.17: component instantiate-args (type 0x03 + func 0x01) and top-level
    func imports, so a resource-type-and-constructor-parameterized nested
    component instantiates against the host resource (beeeba3).
- **Historic fused sub-features** (all now run; the reworks fixed them):
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
- **Filesystem:** rename-at (file + dir subtree), create/remove-directory-at, link-at (real hard link). `TestRealFSOps`, `TestRealHardLink`. Backed by **real mounts**: `WASIConfig.FS` is a `wazy.FSConfig`, the same one the core preview1 runtime takes, so `WithDirMount`/`WithReadOnlyDirMount`/`WithFSMount`/`WithSysFSMount` all work and each mount becomes its own preopened descriptor (the guest does the longest-prefix match). Replaced the former flat in-memory `map[string][]byte` with its synthetic directories. `TestRealMultiFS` (`real_multifs.component.wasm`) is the real-guest proof: three preopens, same basename in each, vs `wasmtime run --dir root::/ --dir tmp::/tmp --dir pkg::/site-packages`.
- **Path escape is refused**, in both runtimes and with no opt-out: a guest path is cleaned and `fs.ValidPath`-checked against the descriptor it resolves against, so it cannot leave that descriptor — not to the preopen root, not off the mount. preview1 has always done this (`atPath`); the component path does it now too. wasmtime agrees byte-for-byte, including refusing `/tmp/../a.txt` where the cleaned path exists in *another* preopen (`TestRealMultiFS`'s `escape_sideways_err=true`). The check is lexical: a symlink inside a mount pointing out of it is still followed. Closing that means rebuilding `sysfs.dirFS` on Go 1.25's `os.Root` (`openat`/`RESOLVE_BENEATH`) — not done.
- **UDP server:** `[method]udp-socket.local-address` (receive-from-anyone + send-to-sender already worked). `TestRealUDPServer`.
- **wasi:random complete:** get-random-u64, get-insecure-random-bytes/u64, insecure-seed (all crypto/rand). `TestRealRandom`.
- **Socket options:** all tcp/udp setsockopt-style setters (keep-alive, buffer sizes, hop limits) as no-op-Ok (spec permits ignoring these advisory hints) — nothing traps.
- **Capstone:** `TestRealMega` — one guest crossing args/env/random-HashMap/stdin/filesystem/clocks, byte-exact vs wasmtime.
- **Remaining fail-loud (by design):** `wasi:filesystem` symlink-at / readlink-at — symlink CREATION is nightly-only in rust std on wasip2 (`symlink_path` unstable), so no stable-rust guest reaches them. Implement if a non-rust guest ever needs them.

## OPEN BUG: intermittent `*Instance` memory corruption on Windows teardown

**Symptom.** `TestAsyncWastConformance/trap-if-sync-and-waitable-set` faults
during teardown on `windows-2025`, roughly 1 CI run in 15, and passes on
re-run. The crash is always at `instance.go`'s `in.amu.Lock()` in
`(*Instance).Close`, reached from the `subInstances` loop.

**It is memory corruption, not a nil pointer.** Three symptoms, one object:

- `fatal error: sync: inconsistent mutex state` with a **valid, non-nil**
  receiver — the object exists, its mutex word is garbage.
- The same object printing as `0x0`, faulting at `addr=0x1`.
- Under `GODEBUG=clobberfree=1`:
  `runtime: marked free object in span ... elemsize=16` — the GC itself
  finding an object freed while still referenced.

The `FSContext` nil deref and the wild pointer inside
`hostModuleInstance.Close` seen in earlier reports are downstream of this,
not separate bugs.

**The receiver is a real object; the write lands inside it.** In the
`inconsistent mutex state` trace the sub-instance is `0x33a5ad497740` and the
mutex `lockSlow` faults on is `0x33a5ad49774c` — offset `0xc`, which is
exactly `unsafe.Offsetof(Instance{}.amu)` (verify with a throwaway test;
`unsafe.Sizeof(Instance{})` is 352). So this is not a garbage or stale
`*Instance` pointer being dereferenced: it is a correctly-aligned, live
`*Instance` whose 4-byte mutex word has been overwritten by a stray write.
Note the GC's `elemsize=16` is therefore a *different* object — whatever
16-byte allocation the same stray writer also hit.

**Ruled out, with evidence:**

- **Not the JIT.** Swapping the native engine for the interpreter still
  crashes. So not codegen, not executable-memory lifetime, nothing in
  `internal/engine/native`. (Re-confirmed independently: an interpreter shard
  crashed in the 16-shard run below, alongside native ones.)
- **Not a data race.** The `-race` shard crashed with no race report.
- `instantiateNestedInstances` cannot store a nil: it appends only after an
  explicit error check, and `subInstances` is written once and never
  mutated. The bad pointer appears *after* the slice is built.

`internal/component/instance` contains no `unsafe`, so with the JIT
eliminated the corruptor is most likely in `internal/wasm`.

**Every victim so far reads back as ZERO.** Not clobberfree's `0xdeadbeef`,
not random garbage — zero, i.e. memory that was freed and re-handed-out as a
fresh (zeroed) allocation. The known victims:

- a slot in the `[]*Instance` `subInstances` backing array (nil receiver in
  `Close`);
- a `*compiledModuleWithCount`'s embedded `*compiledModule` (`addr=0x0`,
  `addr=0x3`);
- a `GoModuleFunc` value: `Exception 0xc0000005 PC=0x0` at `api/wasm.go:479`
  from `(*guestTask).firstRunBody` — a host function whose closure was zero,
  so the engine jumped to address 0;
- an `*Instance`'s mutex word, and a map header's flags.

An independent audit (Codex, xhigh) separately eliminated: the
compiledModule refCount protocol (balanced for this suite), executable
lifetime (every instantiated native `moduleEngine` holds a strong
`*compiledModule`), and `TableInstance.involvingModuleInstances` retention
through the merged-graph import path (the passthrough shim and `$m` both
import the same table and are appended to the retention list before `$m`
installs active elements). It also confirmed the interpreter reaches no Win32
I/O and no executable mapping in this fixture.

- ⚠️ **RETRACTED: Green Tea GC, the executables finalizer, and
  engine-independence.** Each was "ruled out" by a 16-shard dispatch in which
  a shard running the mode still crashed. Those conclusions are **invalid**,
  because crashes turn out to be a property of the RUNNER (see the poisoned-
  runner note below), and every mode ran on a different runner. Those sweeps
  compared machines, not modes. All three hypotheses are open again, and
  `zz-repro.yaml` now probes a runner first and sweeps every mode on that one
  box so the comparison is actually controlled.
- **Not `internal/platform/time_windows.go`.** Its
  `uintptr(unsafe.Pointer(&counter))` into `LazyProc.Call` *looks* like the
  classic pointer-to-uintptr violation but is not one:
  `syscall.LazyProc.Call` is `//go:uintptrescapes`, which forces the operand
  to the heap and keeps it alive for the call.

**Bisected to `a6c208d`** ("perf(component): cut async per-call allocations
(FirstLight 4->1, AwaitImport 11->5)", #4) — the first commit after the async
runtime landed. Clean walk on a poisoned runner: `ee91a4e` 15/25, `e9c123e`
22/25, `2011b15` 16/25, `b41d138` 19/25, `a6c208d` 17/25, with `53e8e4d`
good.

**Read that carefully: it EXPOSES, it does not necessarily cause.** `a6c208d`
is pure Go with no `unsafe` in it — co-allocating task+guestTask in a
`callbackFrame`, an inline `task.resultBuf [1]abi.Value`, and closure
destructuring. Go is memory-safe, so none of that can corrupt a heap on its
own; what it does is cut the async path from 4 allocations to 1, which moves
every object's neighbours and changes when the GC runs. So it is the commit
that made a latent defect visible. Reverting it would hide the bug, not fix
it — don't.

What it points at is the one class of pointer in this repo the GC cannot
see, shared by BOTH engines (which matters, because the interpreter corrupts
at the same rate): `wasm.TableInstance.References` is `[]uintptr`, and
`functionFromUintptr` / `LookupFunction` resurrect a `*function` out of it,
then read `tf.moduleInstance` — a real pointer — out of that. If the
`involvingModuleInstances` retention edge is ever short, those `function`
objects are collectable while the table still addresses them, and reading a
pointer field out of one stores a pointer to freed memory into live memory.
That is precisely "marked free object in span". Next step is to test that
directly (make the resurrected references GC-visible and see whether the rate
drops to zero on a poisoned runner) rather than to reason about it further.

**A runner is either poisoned or clean — this is not a probability.** The
single most important measurement so far. A dispatch that launched many short
processes per shard produced: 15 shards with **0 crashes across 2888
launches**, and one shard with **3 crashes in its first 4 launches** (it
stopped at 3 by design). So a given runner either reproduces almost every
launch or never reproduces at all. That reframes "~1 CI run in 15" as the
fraction of poisoned runners, explains why the crash always lands in the
first 100ms, and explains why a real Windows 10 laptop never reproduces.

It also means **any experiment that compares modes across shards is
confounded**, which is how three hypotheses were wrongly closed. Compare
modes only within one runner.

Poisoned-ness tracks the CPU vendor. Across 48 fingerprinted runners: every
poisoned one was an Intel Xeon (Platinum 8573C / Emerald Rapids, and 6973P-C
/ Granite Rapids); all 40 AMD EPYC runners (7763, 9V74) were clean, as were
the 8370C ones. One 8573C measured clean, so the SKU is not the whole story —
microcode is the untested axis and the fingerprint now records it.

But the machine is not the bug. On a poisoned runner: `cpu.all=off` (every
CPU-feature path in the Go runtime disabled) still crashes 19/25; a no-wazy
control doing pointer-dense allocation, forced GC, goroutines on unbuffered
channels and finalizers runs 0/40; `internal/wasm` runs 0/15; and
`internal/engine/interpreter` runs 0/15 — while `internal/component/instance`
runs 23/25. Intel exposes it; the defect is ours.

**The map crashes are victims, not races.** `concurrent map read and map
write` on the engine's compiled-module map, at `interpreter.go:96` and
`engine_cache.go:115`, both reached from `buildMergedCanonHostModule`
(graph.go:1799). Neither is a real race: all accesses to both maps correctly
hold the engine mutex, and under `GOTRACEBACK=system` (which, unlike `all`,
shows runtime-internal goroutines) every one of the 9 live goroutines is
accounted for and idle — finalizer and cleanup included. So `hashWriting` was
set on a corrupted map header. That map is simply the busiest allocation in
the hot path, i.e. the likeliest victim.

**A DEP violation is in the mix.** One crash is `Exception 0xc0000005` with
first parameter `0x8` — attempted EXECUTE on non-executable memory — at
`PC=0x7ffab0a8f940`, outside the wazy image entirely, unwinding from
`DeleteCompiledModule`'s deferred unlock. Another is a wild
`0xffffffffffffffff` fault in `Close`'s `closers` loop. Both from the same
poisoned runner within four launches.

**The unit of risk is a PROCESS START, not an iteration.** Every crash
captured so far died between 0.01s and 0.115s — inside a `-count=2500` run,
i.e. during its FIRST iteration. Four dispatches produced 9 crashes from ~64
starts: roughly 1 in 7. So cranking `-count` bought nothing; each shard was a
single sample. `zz-repro.yaml` now launches many short processes per shard
and reports crashes/starts, which turns a dispatch into a rate rather than a
coin flip — the thing a bisect needs as an oracle.

**Reproducing.** It does not reproduce on Linux — ~10,000 iterations across
plain, `-race`, `-cpu` variants, `clobberfree`, `gccheckmark`, and
interpreter mode are all clean. It also does **not** reproduce on a real
Windows 10 Pro (19045) laptop: ~1600 process starts across plain, `GOGC=1`,
`clobberfree`, `gccheckmark`, `GOMAXPROCS=2`, 48-way oversubscription and
both Go 1.26 patch levels, all clean, against CI's 1-in-7. Whatever the
trigger is, it needs the `windows-2025` runner image. Note a cross-compiled
`go test -c` binary runs standalone there (fixtures are `//go:embed`ed), so
testing on a Windows box needs no Go install and no repo checkout — just
`GOOS=windows go test -c` and copy the exe.
`.github/workflows/zz-repro.yaml` (manual dispatch only) is the vehicle.

**Read this before dispatching it again.** The previous harness inlined
`head -n 160` of the failing log, and the clobber shard's crash *opens* with a
GC span dump of hundreds of `0x… alloc unmarked` lines — 159 of the 160
captured lines were that dump, so the `fatal error` and stack trace right
behind it were never printed. The clobber shard is precisely the one that
faults at the culprit instead of at a later victim, and its evidence was lost
that way. The current workflow therefore uploads the raw log as an artifact
unconditionally and strips the span dump before inlining anything. Fetch the
artifact, not the job log.

**Not this.** Four real bugs were found while investigating and are fixed
(`FailIfClosed` re-running the deferred cleanup and over-decrementing
`mem.importers`; a memory view captured before a guest call and used after
it; `reapParkedGoroutines`' never-spawned arm freeing a thread-table slot on
the *reaper's* instance instead of the thread owner's, so one thread index
could be handed out twice); and `writeSocket` in `internal/sysfs/
file_windows.go` returning on `ERROR_IO_PENDING` while the kernel still held
pointers to `&overlapped`, `&done` and `buf`, so the kernel wrote into Go
memory after it was recycled. None explains this crash — it predates all
four; the third is inert on the crashing suite, where the reaper and the
thread's owner are the same instance; and the fourth needs socket I/O, which
the async wast fixture never performs. The fourth is worth noting anyway: it
is the exact *mechanism* this bug must have (a kernel-side write into
recycled Go memory, invisible to `-race` and checkptr), so look for more
Win32 calls that hand the kernel a pointer and return before it is done.

**Do not "fix" it defensively.** A `if sub == nil { continue }` in the
`subInstances` loop makes the symptom disappear while leaving live-memory
corruption in the engine. The next step is identifying the corrupting
write, not silencing the reader.

## Per-call realloc closure alloc (deferred — low ROI)
- **What:** `invoke` builds a fresh `abi.Realloc` closure (capturing `ctx`) on every call. It stays on the stack for calls that never touch memory (CallAdd), but escapes to the heap on any string/list parameter (one alloc/call, e.g. CallGreet).
- **Why deferred:** killing it means threading `ctx` through `abi.Realloc` and ~20 store/lower functions in abi (memory.go + flat.go) so the closure can be built once at bind time. That's a wide signature sweep for a single alloc on the string path — poor ratio next to the two wins already taken (lift-iterator pool + top-level-primitive fast path: CallAdd 5→2 allocs, ~245→177 ns/op). Revisit only if string-heavy call profiles demand it.

## DONE: multi-component composition (wasmtime model) + single instantiation path
- A Runtime now hosts any number of component instances (distinct AND multiple of the same), and one component can call another on one Runtime (`TestOneComponentCallsAnotherOnOneRuntime`, `TestTwoLogHelloCoexist`). The graph engine instantiates internals anonymously (`WithName("")`) and wires them via a per-instantiation `experimental.ImportResolver`, so nothing internal touches the global registry -- like wasmtime. Compile cache intact (empty-import rewrite → stable `graphEmptyImportKey`).
- The old `instantiateWithImports` path is DELETED (712 lines incl. its exclusive helpers + ~29 old-only tests); all host-import components now route through the graph engine. Only `instantiateComponent` (trivial single-module, no-import) remains beside it. See the [[wasip2-component-model]] memory for the full mechanism.
