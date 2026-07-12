# Optimization Scan

Baseline: wazero upstream `c0f3a4e` ("module: only release owned memory on module close").
Method: six subsystem scans (compiler backend, interpreter, runtime core, WASI/sysfs, API/cache
layer, hostcall mechanism). Findings marked **measured** were verified with pprof, `-gcflags=-m`
escape analysis, `-d=ssa/check_bce`, or in-repo benchmarks on this checkout.

## Top priorities (cross-cutting, ranked)

| # | Finding | Area | Impact |
|---|---------|------|--------|
| 1 | Reflect-wrapped host functions 14.5x slower than direct (1086 vs 75 ns/op, 6 allocs) — no fast-path adapters for common signatures | hostcall | high, **measured** |
| 2 | Interpreter: heap-allocated `*callFrame` per call (~1 alloc/call; 1.35M allocs/op in fib) | interpreter | high, **measured** |
| 3 | Interpreter dispatch loop too big for Go inliner — `popValue` alone 13.6% CPU as a real call | interpreter | high, **measured** |
| 4 | `fd_readdir`: one lstat(2) + FileInfo alloc per dirent (~11x slower than needed) | sysfs | high, **measured** |
| 5 | `try_table` enter: Go-exit + full stack clone + 1KB register copy on the non-throwing path | compiler | high |
| 6 | `ExportedFunction()` allocates a fresh callEngine + 10 KiB zeroed stack every lookup | API | high |
| 7 | File-cache hit: one 8-byte read(2) syscall per function offset (no bufio) + full revalidation of an already-validated binary | cache | high |
| 8 | Binary decoder reads byte-by-byte through `bytes.Reader`/closures instead of slice indexing | decode | high |
| 9 | Element segments: heap `ConstantExpression` per funcref entry, re-parsed 3x (decode/validate/instantiate) | runtime | high |
| 10 | `maps.Clone` per `block`/`loop`/`if` during validation, even when map is empty | validation | high |
| 11 | No SSA constant folding / CSE / copy-prop (pass list is a TODO); amd64 emits 2 defensive reg copies per ALU op; arm64 never uses scaled addressing | compiler | high |
| 12 | `ctx.Value(EnableSnapshotterKey{})` context-chain walk on every call — in wazevo entry, interpreter (per frame!), and hostcall paths | all engines | med-high, **measured** (5.6% cum) |

---

## 1. Hostcall mechanism

Measured baseline (amd64): direct `api.GoModuleFunc` round trip **74.9 ns/op, 0 allocs**;
the original reflection-based host-function registration **1086 ns/op, 6 allocs** (14.5x).

- **H1. Reflect path has no fast path** — historical: per call the reflection path did `make([]reflect.Value, pLen)`, `reflect.New(t).Elem()` per param, `newContextVal`/`newModuleVal`, and `fn.Call`. Impact: high, **measured**, certain.
  - **RESOLVED**: reflection was removed entirely. Host functions are now registered either through the explicit `WithGoModuleFunction`/`WithGoFunction` stack API or through the typed generic helpers `wazero.HostFunc0`-`HostFunc8` / `HostProc0`-`HostProc8` (`host_typed.go`), which derive the WebAssembly signature from Go's type system and decode/encode the stack directly. Both are zero-allocation and at parity with direct `GoModuleFunc`. The interim generated reflect-adapter matrix (`gofunc_fast.go`) was deleted along with the reflect registration path.
- **H2. itab hash lookup per host call** — **RESOLVED**: `buildHostModuleOpaque` now type-switches once and stores the asserted `api.GoModuleFunction`/`api.GoFunction` interface words; `hostModuleGoFuncFromOpaque[T]` reinterprets without `assertE2I`. Combined with H5+A7: gomodule CallWithStack 82→74 ns, now ~11.5% faster than upstream.
- **H3. Trampoline saves 23 regs one at a time (arm64)** — **RESOLVED**: save/restore now pairs adjacent same-type registers via new `stp`/`ldp` signed-offset encodings (`storeP128`/`loadP128` kinds); go-call trampoline 23→12 memory instrs each way, stack-grow 39→20. Validated by ARM ARM hand-computed encoding tests, independent disassembly, and full spectest v1/v2 + engine tests on arm64 under qemu. amd64 has no pair-store; left unchanged. (C9 prologue pairing can reuse the new encodings.)
- **H4. Listener path eagerly unwinds the whole stack** before `listener.Before` even if unused; per-call `FunctionDefinition` lookup. `call_engine.go:399-460`. Fix: lazy iterator, cache defs. Impact: medium (listener users only), certain.
- **H5. Non-inlinable closure + conditional defer wraps every host call** — **RESOLVED**: the four dispatch sites branch to named `callGo*WithSnapshotRecover` functions only when the snapshotter is enabled; common path calls `f.Call` directly (no closure, no defer).
- **H6. `ensureTermination`: goroutine + channel per wasm entry; loop-header check compiles to a full Go-exit round trip per loop iteration** — `module_instance.go:32-41`, `frontend/lower.go:1368-1379`. Fix: native load+branch on a closed flag; check `ctx.Done()` at existing dispatch instead of spawning a watcher. Impact: medium for `WithCloseOnContextDone` users, likely.
- **H7. Trampoline stack-bounds check** (4 instrs/call) could be elided for small frames via an invariant slack margin. Impact: low-med, speculative.
- **H8. Interpreter: per-hostcall interface type switch** — `interpreter.go:871-877`. Normalize to `GoModuleFunction` at compile. Impact: low, certain.
- Verified good: exit mechanism is store+ret (no thread/goroutine switch); one copy each way for params; listener-ness baked into exit codes (zero cost when unlistened); no `context.WithValue` per hostcall.

## 2. Interpreter (`internal/engine/interpreter`)

pprof on fib: `callNativeFunc` 59% flat, `popValue` 13.6%, `pushValue` 6.8%, malloc ~10%.

- **I1. `*callFrame` heap alloc per call** — **RESOLVED**: `frames []callFrame` value slice; frame pointer re-taken after every nested call. fib_for_30 allocs/op: 1,346,273 → 2.
- **I2. Dispatch func is 'big' (cost 56600) → inliner cap drops to 20** — **RESOLVED**: SIMD/atomics/bulk-memory cases (79 ops) split into `callNativeFuncRare`; cost 56600 → ~11350, `popValue`/`pushValue`/`drop`/`popMemoryOffset` (289 sites) now inline into the dispatch loop.
- **I3. `frame.pc` lives on heap** — **RESOLVED**: local `pc` register mirror with documented sync policy (traps/calls/throws sync, ALU/branch path store-free); loop guard indexes `len(body)` directly so the per-instruction bounds check is eliminated. Trap stack traces verified byte-identical. I1+I2+I3 combined: fib_for_20 ~1.8 ms → ~1.05-1.2 ms, fib_for_30 ~184-211 ms → ~125-147 ms (~30% faster), allocs flat 2/op.
- **I4. `ctx.Value(EnableSnapshotterKey{})` per call instruction** — `interpreter.go:252`. Fix: compute once per activation. Impact: med-high, certain.
- **I5. Label ops compiled in and executed as no-ops**; `br`-to-next pairs emitted (`compiler.go:506,739,673`). Fix: strip labels + remap PCs in `lowerIR`; peephole fallthrough brs. Impact: medium, certain.
- **I6. Two-level dispatch** — **RESOLVED**: `lowerIR` specialization pass rewrites generic ALU/compare/load/store kinds into 72 monomorphic kinds; inner type switches deleted from dispatch. Hot/rare split rebalanced with dynamic-frequency data (no relocated op >0.26% in real workloads).
- **I7. `LabelCallers` map written per branch, never read** — `compiler.go:258` + ~20 sites; cleared with per-key deletes. Fix: delete it. Impact: medium (compile time), certain.
- **I8. `unionOperation` is 56 B**; `Us []uint64` header dead for ~95% of ops. Fix: side pool → 32 B ops. Impact: medium, likely.
- **I9. Per-call chain `callWithUnwind`→`callFunction`→3-way dispatch**, all non-inlinable, static per callee. Fix: callee-kind tag, direct dispatch. Impact: medium, likely.
- **I10. Memory ops: double bounds check + 2 non-inlined calls per load** — **RESOLVED**: specialized load/store cases use one merged bounds check (proven equivalent incl. the 4GiB boundary) + direct `binary.LittleEndian` access. I6+I10+I11 combined: ~3.9% geomean on interpreter benchmarks (interleaved benchstat; smaller than scanned estimate because I1-I3 already inlined the helpers these paths used).
- **I11. Global get/set chases `g.Type.ValType` to detect V128** — **RESOLVED**: V128-ness precomputed into `op.B3` at lowering.
- **I12. `BrIf` calls non-inlined `drop` even for the no-drop common case** — check the -1 sentinel inline. Impact: low-med, certain.
- I13/I14 (compile-time): `lowerIR` zero-extend temp allocs; `Next()` per-key map clear. Low.

## 3. Compiler backend (`internal/engine/wazevo`)

- **C1. `try_table` enter/leave: Go-exit + `cloneStack` (≥10KB) + 1KB `savedRegisters` copy per dynamic entry**; every branch out of a catching frame pays another round trip — `call_engine.go:608-657`, `frontend/lower.go:4672-4725`. Fix: PC-range side table resolved at throw time (native EH model); at minimum defer clone to throw. Impact: high (EH workloads), certain.
- **C2. amd64: 2 defensive reg copies per binary ALU/shift/xmm op** — `amd64/machine.go:1710-1804`. Fix: write into rd directly when source dies. Impact: high (compile time + residual moves), certain/likely.
- **C3. arm64: `Ishl` never folded into scaled addressing** (in-code TODO, `lower_mem.go:271`) — every scaled array access = lsl+add+ldr instead of `ldr [base, idx, lsl #n]`. Impact: high, certain.
- **C4. No const-fold/GVN/CSE/copy-prop SSA passes** (`ssa/pass.go:34-40` TODO; only shift-by-0 elim exists). Fix: cheap dominator-order local GVN over existing `InstructionGroupID` + `b.alias`. Impact: high, certain.
- **C5. `passRedundantPhiEliminationOpt` is fixed-point over all blocks** (author-measured 22 iterations on sqlite). Fix: worklist. Impact: med-high (compile time), certain.
- **C6. amd64 `lowerExtend` allocates temp + redundant move per extend** — `machine.go:1359-1397`. Impact: medium, certain.
- **C7. amd64: commutative ops never swap const into imm32 slot** (TODO at `machine.go:1703`). Impact: medium, certain.
- **C8. arm64: constants re-materialized per use (up to 4 instrs); FP consts embed an executed branch-over-literal, never deduped** — fix: memoize per function; constant pool after epilogue (amd64 already does). Impact: medium, certain.
- **C9. arm64 prologue/epilogue: no stp/ldp pairing** (TODOs at `machine_pro_epi_logue.go:75,288`) — 2x instrs + 2x frame size per callee-saved reg. Impact: medium, certain.
- **C10. `Call` allocates `paramResultSlice` per invocation** — `call_engine.go:193`. Measured +21ns/+27% vs `CallWithStack`. Fix: reusable buffer on callEngine (Call is documented non-concurrent) — but results alias next params; needs care or doc cross-ref. Impact: medium, certain.
- **C11. try_table: `local.set` in try body emits fresh save-area-ptr load each time** (no CSE to clean up) — `lower.go:1106-1119,4614`. Fix: SSA variable per block. Impact: medium (EH), certain.
- **C12. Regalloc: fixed 64-slot scans** in `releaseCallerSavedRegs` (per call instr!), `fixMergeState` (per edge), `scheduleSpill`. Fix: occupancy bitmask + `bits.TrailingZeros64`. Impact: med-low, certain.
- **C13. `ssa.builder.Init`: O(nextValueID) map deletes because debug annotations always populated** (`builder.go:293`, `frontend.go:378`); signatures map iterated per compile to reset a debug-only flag. Fix: `clear()`, gate behind debug flags. Impact: med-low, certain.
- **C14. `spillSlots` map + 3-pass manual clear in Reset** (both ISAs). Fix: slice by VRegID. Impact: low-med, certain.
- **C15. `basicBlock.addPred` O(preds²) debug guard in prod path** — br_table trampolines create thousands of preds. Fix: gate behind `SSAValidationEnabled`. Impact: low-med, certain.
- **C16. Euler-tour + RMQ dominator table built per function, consumed only on spill reloads**; re-inits at previous max size; `math.Log2` instead of `bits.Len32`. Fix: lazy build. Impact: low-med, likely.
- **C17. Machine code copied twice per function** (TODO at `engine.go:540`). Impact: low-med, certain.
- **C18. arm64 `resolveRelativeAddresses` restarts full walk per trampoline insertion.** Impact: low, likely.
- C19/C20: amd64 10-byte movabs for u32-range consts, zero-base-reg materialization, per-byte const emission; regalloc liveness dup appends (TODO). Low.

## 4. Runtime core (`internal/wasm`, decode/validate/instantiate)

- **R1. Element segments: heap `ConstantExpression` per funcref entry** — decode re-encodes the index via LEB (`binary/element.go:23-42`), then `evaluateConstExpr` re-parses it with 2 heap stacks per entry, ×3 (validate, declared-indexes, instantiate). Fix: `[]Index` representation with sentinel/flag (upstream wazero's shape) or non-allocating fast path. Impact: high, certain.
- **R2. All decoding via `bytes.Reader` byte-at-a-time** — **RESOLVED**: whole binary package rewritten to `(buf, offset)` slice threading; leb128 `Load*` are closure-free primitives with folded overflow checks (proven by frozen-algorithm differential test). Plus code-body arena + single-pass locals (R4), string arena + import-module interning (R13), `strings.Builder` type keys (R9 — lazy rejected: concurrent first-use via emscripten/table paths), decode micro-allocs (R15/R16). DecodeModule: -27.5% ns/op interleaved (up to 1.5x quiet), allocs 456 → 132 (-71%).
- **R3. `maps.Clone(sts.initLocals)` per control op in validation** (`func_validation.go:1727,2116,2133,2158`) — allocates even when empty (the ~always case). Fix: nil-if-empty or undo journal. Impact: high, certain.
- **R4. `decodeCode`: body copied per function + locals parsed twice** — **RESOLVED** (with R2): one arena per code section, single-pass locals. decodeCode alloc share: 40% → 1.9%.
- **R5. `ValueType` widened to uint64 (8x memory)** on every type slice/validation stack (`module.go:1196`). Fix: pack to uint32 or 1-byte kind + sparse side table. Impact: med-high, certain (repr) / likely (effect).
- **R6. `evaluateConstExpr` allocates 2 stacks even for `i32.const`** — per global/data/element at instantiate. Fix: fast-path 1-instruction forms. Impact: med-high, certain.
- **R7. `buildFunctionDefinitionsOnce` O(functions × exports)** (`function_definition.go:105`) + `paramNames` rescan per function. Interpreter triggers it per compile. Fix: one-pass export map, cursor over sorted names. Impact: medium, certain.
- **R8. `typeIndexOfFunction` linear scan of imports → O(imports²) in `resolveImports`** (`module.go:250`). Fix: precompute type-index slice at decode. Impact: medium, certain.
- **R9. `FunctionType.key()` string `+=` per element, forced eagerly at decode** (`module.go:860`, `binary/function.go:53`); same in `structuralTypeKey`. Fix: `strings.Builder` + defer to first use. Impact: med-low, certain.
- **R10. Block-type decode round-trips a `bytes.Reader` per control opcode** (`func_validation.go:1721+`); one-byte fast path possible. Impact: medium, certain.
- **R11. Memory `Grow` beyond cap = append-driven full copies (~1.25x steps)** (`memory.go:263`). Fix: aggressive cap rounding or mmap-reserve to max (shared memory already does). Impact: medium, likely.
- **R12. Import module-name strings not interned** (hundreds of copies of "env"/"wasi_snapshot_preview1"). Low-med.
- **R13. Per-instantiate O(exports) map scans for table exports / memory defs** (`store.go:375`, `module_instance.go:191`). Low-med.
- **R14. leb128 signed decoders: redundant terminal-byte overflow work (fixme comments) + `fmt.Errorf` on probe paths.** Low-med.
- **R15. Misc decode micro-allocs**: 1-elem slice per global type, temp bufs for f32/f64/v128 consts, magic/version via ReadFull, element-wise externref zeroing (use `clear`). Low.

## 5. API / cache / instantiate layer

- **A1. `ExportedFunction()` = new callEngine + ≥10 KiB zeroed stack per lookup** (`module_engine.go:162-226`, `call_engine.go:150-174`). The `mod.ExportedFunction("f").Call(ctx)` per-request pattern pays ~10.3 KB garbage each time. Fix: lazy stack alloc + `sync.Pool` by size class + cached per-export Function. Impact: high, certain.
- **A2. File-cache deserialize: unbuffered `*os.File` → 8-byte read(2) per function offset / source-map entry** (`engine_cache.go:241-363`). Fix: one-line `bufio.NewReaderSize`; also `reader.Read` → `io.ReadFull` (short-read bug). Impact: high, certain.
- **A3. Cache-hit `CompileModule` still fully decodes AND validates every opcode before checking the cache** (`runtime.go:229-266`). Fix: hash + probe cache first, skip body validation on hit. Impact: high (cache-hit startup), likely.
- **A4. Reflect host functions** — see H1. Also `newContextVal`/`newModuleVal` → `reflect.ValueOf` is a trivial 2-4 alloc win.
- **A5. `serializeCompiledModule` builds whole artifact in memory (full executable copy, no `Grow`)** then triple-passes it; plus fsync per Add. Fix: `io.MultiReader` zero-copy. Impact: medium, certain.
- **A6. Every instantiate builds a ~4.9 KB `math/rand` source (607×int64 + ~1800 seed iterations) + fake-clock closures** (`config.go:846`, `platform/crypto.go:15`). Fix: lazy or 16-byte splitmix64/PCG. Impact: med-high for instantiate-per-request, certain.
- **A7. Snapshotter `ctx.Value` on every call** — **RESOLVED**: `expctxkeys.SnapshotterEnabled` atomic latch set in `experimental.WithSnapshotter` gates every hot-path `ctx.Value(EnableSnapshotterKey)` in wazevo entry and both interpreter sites. (Interpreter per-activation caching from I4 still open.)
- **A8. DWARF on by default: all `.debug_*` sections copied + `dwarf.Data` eagerly parsed per compile** — consumed only for error stack traces. Fix: subslice + lazy parse. Impact: medium (debug-built guests), certain.
- **A9. Engine takes write lock for the read-only instantiate-path lookup** (`engine_cache.go:101`, mux is RWMutex). Fix: RLock / atomic refcount. Impact: medium under concurrency, certain.
- **A10. File-cache hit still allocates full SSA builder + backend compiler just for entry preambles** (`engine_cache.go:76`). Fix: serialize preambles or share engine-level compiler. Impact: medium, likely.
- **A11. Host-module instantiation builds a full (unused) sys.Context** — pays A6 for nothing (`runtime.go:320`). Fix: skip for host modules. Impact: low-med, certain.
- **A12. `WithEnv` O(n²) clone-per-var; environ re-encoded per instantiate.** Low.
- **A13. `InstantiateModule` mutates the caller's documented-immutable `ModuleConfig`** (`runtime.go:317` sockConfig) — race; fix without deep-cloning environKeys. Correctness, certain.
- **A14. `GetFunctionTypeIDs`: per-type mutex round-trips + `+=` key building.** Low.

## 6. WASI / sysfs

Verified good: fd_read/fd_write/fd_seek/clock_time_get are zero-alloc; iovecs slice guest
memory without copying; fd table lookup is lock-free inlined bitmask; WASI binds via direct
`GoModuleFunc` (no reflection).

- **W1. `fd_readdir`: lstat(2) + FileInfo alloc per entry** (`sysfs/file.go:501` uses `os.File.Readdir`; stdlib lstats every name). Measured 16,653 ns vs 1,502 ns embed.FS for 13 entries. 8KB guest buffer → up to ~339 lstats per call. Fix: parse getdents64 directly (`syscall.ReadDirent`), or `ReadDir` (DirEntry mode) + lazy ino. Impact: high, **measured**, certain.
- **W2. `AdaptFS.Stat` = open+fstat+close** for `fs.FS` mounts (`adapter.go:39`); measured 3,751 vs 1,376 ns. Fix: `fs.StatFS` fast path. Impact: high, certain.
- **W3. `poll_oneoff` polls blocking fds sequentially, each with the full remaining timeout** (`poll.go:181-210`) — latency bug + N ppolls; multi-fd `_poll` already exists internally (`poll_linux.go:30`). Fix: batch into one ppoll. Impact: high for socket guests, certain.
- **W4. Socket read/write: `SyscallConn()` + 2 escaping closures per call** (~3-4 allocs) (`sock.go:102-154`). Fix: cache RawConn/fd at construction. Impact: med-high, certain.
- **W5. Every Stat = `os.fileStat` alloc + fresh `*cachedStat` alloc** (`osfile.go:174-187`). Fix: `syscall.Fstat` into value field. Impact: medium, certain.
- **W6. `fd_fdstat_get` does a full fstat just for the immutable filetype** (`fs.go:186-234`). Fix: cache filetype on `FileEntry`. Impact: medium, certain.
- **W7. `atPath` allocates 1-3 strings per path_* call**, plus `dirFS.join` concat + `BytePtrFromString` — 3-4 copies of the same bytes. Fix: single-buffer build. Impact: medium, certain.
- **W8. `poll_oneoff` heap-allocates one `*event` per subscription** (`poll.go:109`, escape-verified) — this is every sleep/timer tick in Rust/TinyGo guests. Fix: write records into outBuf directly. Impact: medium, certain.
- **W9. DirentCache first-read reallocates the window; dot entries force growth** (`sys/fs.go:184-201`). Low-med.
- **W10. `path_open` re-looks-up the fd it just created** for the O_DIRECTORY check. Low.
- **W11. `fd_prestat_dir_name` copies via throwaway `[]byte`** — use existing `WriteString`. Low.
- **W12. `random_get` = one getrandom(2) per guest call** with real entropy configured; batch/buffer. Low-med.
- **W13. `poll_oneoff` zeroes the whole out buffer though writeEvent overwrites most of it.** Low.
- **W14. `fdReaddir` over-requests entries vs what the buffer holds** (multiplier for W1). Low.

---

## Suggested attack order

1. **Cheap + certain, no design work**: A2 (bufio, one line), R3 (maps.Clone guard), I4/A7 (snapshotter latch), H5 (defer split), W11, C13 (`clear()`), gofunc `reflect.ValueOf` swap.
2. **High-value contained rewrites**: I1+I2+I3 (interpreter call path — compound on same hot path), H1 (host-fn fast-path adapters), W1 (getdents), A1 (stack pooling), R2 (slice-based decoder), R1 (element repr).
3. **Design-level**: C1 (EH side-table model), A3 (cache-first compile), C4 (SSA GVN pass), H6 (native closed-flag check), W3 (batched poll).

Lock in wins with alloc-count benchmarks (the repo already has `leb128_alloc_test.go` asserting
zero-alloc patterns — extend that to DecodeModule/Instantiate/Call paths).
