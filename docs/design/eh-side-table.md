# Zero-cost `try_table` via a PC-range exception side table (wazevo)

Optimization finding **C1** (and the closely-related **C11**). This document is an
implementation design only — no code is changed by it.

- **C1**: `try_table` enter/leave pays a Go-exit + full stack clone (>=10KB) + 1KB
  `savedRegisters` copy per dynamic entry; every branch out of a catching frame pays
  another Go round trip (`OPTIMIZATIONS.md` line 63; `call_engine.go:605-654`,
  `frontend/lower.go:4671-4725`).
- **C11**: every `local.set`/`local.tee` inside a try body emits a fresh save-area
  pointer load + store (`lower.go:1098-1120`, `4613-4669`).

The goal: the **non-throwing** path of `try_table` should cost (ideally) **zero** extra
instructions for enter and leave — no Go exits, no checkpointing, no stack cloning — so
that C++/Emscripten-style EH-heavy modules that enter `try_table` constantly but rarely
throw stop paying for machinery they never use.

---

## 1. Current implementation, reverse-engineered

### 1.1 Frontend lowering of `try_table` (`frontend/lower.go`)

`OpcodeTryTable` lowering (`lower.go:3550-3723`):

1. Parse the block type and the catch clauses. Each `catchClause{kind, tagIndex,
   labelIdx}` is one of `catch` / `catch_ref` / `catch_all` / `catch_all_ref`
   (`lower.go:3565-3583`). Legacy `try/catch/delegate/rethrow` are rejected at
   validation (`func_validation.go:2294`), so the only dialect we support is
   `try_table` + `throw` + `throw_ref`.
2. Register per-`try_table` metadata and get a module-local **try-table ID**:
   `tryTableMetadata.Append(TryTableInfo{CatchClauses, NumLocals, ReuseLocals})`
   (`lower.go:3593-3598`). `NumLocals = params + locals`. `ReuseLocals` is set when this
   is a nested same-function `try_table` (`tryTableDepth > 0`).
3. Allocate `followingBlk` (join after the `try_table end`) and `bodyBlk`.
4. **If there are catch clauses** (`lower.go:3605-3693`):
   - `storeCallerModuleContext()` so the dispatch loop can find the module.
   - For each catch clause, build a **handler block**: `reloadAfterCall()` (reloads
     memory base/length and mutable globals — `lower.go:4381`), `reloadLocalsFromSaveArea()`
     (see §1.2), load exception params (`loadExceptionParams`, from
     `execCtx.exceptionParamsPtr`) and/or the exnref (`loadExnRef`, from
     `execCtx.exceptionPtr`), then `emitTryTableLeaves(labelIdx)` to pop any enclosing
     handlers the branch crosses, then jump to the resolved wasm target block.
   - Emit the **enter trampoline call**: load `tryTableEnterTrampolineAddress` from
     execCtx and `AsCallIndirect` it with `(execCtx, encodedExitCode)` where
     `encodedExitCode = ExitCodeTryTableEnter | (tryTableID<<8)` (`lower.go:3659-3673`).
   - After the trampoline returns, load `caughtExceptionClauseIdx` from execCtx and
     `AsBrTable` on it: `-1` (or out of range) -> `bodyBlk`; `k` -> handler block `k`
     (`lower.go:3675-3693`).
5. **Body block** (`lower.go:3703-3711`): `reloadAfterCall()`, then
   `storeAllLocalsToSaveArea()` (initialize the save area with current local values),
   `tryTableDepth++`.
6. Push a `controlFrameKindTryTableWithCatch` (or `...TryTable` when no clauses)
   control frame.

**Leave.** At the `try_table end` (`lower.go:1462-1466`) and at every branch that exits a
catching frame (`branchExitsTryTable` / `emitTryTableLeaves`, `lower.go:4688-4725`;
call sites at `1507`, `1532`, `1605`, `3453`, `3646`, `3759`, `3819`, `3866`, `4976`), the
frontend emits `emitTryTableLeave()` — another indirect call to the
`tryTableLeaveTrampolineAddress` (`lower.go:4671-4686`). So both enter and *every* exit of a
catching frame is a Go round trip.

**Throw.** `OpcodeThrow` (`lower.go:3469-3529`): call the `throwAlloc` trampoline to get an
`Exception` sized to the tag (`ExitCodeThrowAlloc`), store each param into
`exceptionParamsPtr[i*8]` (floats bitcast to int), then `emitThrow(exnref)`
(`lower.go:4594-4611`) — an indirect call to `throwTrampolineAddress` followed by an
`ExitCodeUnreachable` (throw never returns). `OpcodeThrowRef` (`lower.go:3531-3547`) checks
for a null exnref (traps via `ExitCodeNullReference`) and calls the same throw trampoline.
There is no `rethrow` opcode in this dialect — "rethrow" is spelled `throw_ref exnref`.

### 1.2 The save-area contract — what it actually reconciles

This is the crux of the current model and the key to the redesign.

`local.set`/`local.tee` inside a try body (`tryTableDepth > 0`) emit, *in addition to*
redefining the SSA local, a store to a **heap-allocated locals save area**:
`storeLocalToSaveArea` = `load execCtx.localsSaveAreaPtr; store val, [ptr + idx*16]`
(`lower.go:1106-1119`, `4622-4629`). On entry the body does
`storeAllLocalsToSaveArea` (`lower.go:4655-4669`); each handler block does
`reloadLocalsFromSaveArea` which `load`s every local back and re-`DefineVariableInCurrentBB`
(`lower.go:4631-4653`).

Why does this exist? Because of **how the runtime catches** (§1.3): on throw, the runtime
**restores a clone of the entire native stack captured at enter time** — exactly like
`experimental.Snapshot`. Restoring the cloned stack rewinds *every stack slot*, including
any spilled locals, back to their **enter-time** values. But the wasm EH spec says locals
are function-scoped and are **not** rolled back when an exception is caught — a handler must
observe locals with their **throw-time** values. The save area is a **side channel that is
not part of the cloned stack** (a separate Go heap buffer), so it survives the stack
rollback and carries throw-time local values into the handler.

Put precisely, the current design reconstructs the handler's live-in state from two sources:

- **Immutable cross-`try` values** (things computed before the `try` and merely passing
  through it): recovered by restoring `savedRegisters` (§1.3) — the enter-time register
  file equals the current value because these did not change.
- **Locals** (which *can* be mutated in the body): recovered from the **save area**, because
  the register/stack rollback would otherwise hand the handler stale enter-time values.

So the save area exists *only* because the runtime rolls the machine state back to the
enter checkpoint. A model that does **not** roll back (unwind-in-place, like the
interpreter) does not need it for correctness — it only needs a memory home for values that
the native unwind would clobber. This observation is what makes both C1 and C11 removable
together.

### 1.3 Runtime enter / leave / throw (`call_engine.go`)

State lives on the `callEngine`:

- `tryHandlers []tryHandler` — a Go-side stack of active handlers (`call_engine.go:45-73`).
  Each `tryHandler` records `{sp, fp, top, returnAddress, savedRegisters [64][2]uint64,
  stack []byte (a full clone), catchClauses, localsSaveArea []uint64, moduleInstance}`.
- `pendingException *wasm.Exception` — GC root for the in-flight exception.

`executionContext` (shared with compiled code, `call_engine.go:76-146`) carries the
trampoline addresses plus `exceptionPtr`, `exceptionParamsPtr`, `caughtExceptionClauseIdx`,
and `localsSaveAreaPtr`.

- **`ExitCodeTryTableEnter`** (`call_engine.go:605-644`): read the try-table ID from the
  trampoline's stack arg; look up `TryTableInfo`; `cloneStack(len(stack)+16)` +
  `adjustClonedStack(...)` (a **full copy** of the live stack, >=10KB); optionally
  `make([]uint64, NumLocals*2)` for the save area (a heap alloc); append a `tryHandler`
  copying the **1KB** `savedRegisters` by value; set `caughtExceptionClauseIdx = -1`;
  resume compiled code via `afterGoFunctionCallEntrypoint`.
- **`ExitCodeTryTableLeave`** (`call_engine.go:645-654`): pop `tryHandlers`, fix up
  `localsSaveAreaPtr`, resume.
- **`ExitCodeThrow`** (`call_engine.go:586-602`): read the exnref from the stack, call
  `doHandleException` (below). If unhandled -> `panic(ErrRuntimeUncaughtException)`.
- **`doHandleException`** (`call_engine.go:665-709`): search `tryHandlers` innermost->
  outermost; match each clause (`catch`/`catch_ref` by tag identity in the *handler's*
  module; `catch_all*` always). On match: restore `localsSaveAreaPtr`, truncate
  `tryHandlers` above the match, set `pendingException`, then **restore the clone** —
  `stack = h.stack`, `stackTop = h.top`, `stackBottomPtr`, `stackPointerBeforeGoCall = h.sp`,
  `framePointerBeforeGoCall = h.fp`, `goCallReturnAddress = h.returnAddress`,
  `savedRegisters = h.savedRegisters` — and write `caughtExceptionClauseIdx = k`. Control
  then returns (via the throw trampoline's normal exit path) into the **enter** site's
  continuation, whose `br_table` dispatches to handler `k`.

The catch mechanism is therefore literally the snapshot/restore machinery
(`snapshot`/`doRestore`, `call_engine.go:856-905`): **enter == snapshot, throw == restore
to the matching snapshot.** `afterGoFunctionCallEntrypoint(executable, execCtx, sp, fp)`
(`entrypoint_arm64.go`, `entrypoint_amd64.go`) is the primitive that jumps to a native
address with a chosen SP/FP after restoring `savedRegisters` — this is the control-transfer
primitive we reuse in the new design.

### 1.4 The interpreter's EH — the reference model we want to port

The interpreter already implements exactly the table-driven model this design proposes,
at frame granularity (`interpreter/interpreter.go:145-250`):

- Each compiled function carries a static `exceptionTable` of entries
  `{startPC, endPC, clauses[]}` plus, per clause, `{kind, tagIndex, targetPC,
  targetStackDepth}` (built at lowering, `interpreter.go:880-887`).
- `searchExceptionTable(exn, frame)` scans the table **backwards** (inner `try_table`s have
  higher indices, so innermost matches first) for an entry whose range contains
  `frame.pc`, then matches clauses (`interpreter.go:220-243`).
- `applyExceptionHandler` truncates the operand stack to
  `frame.base - params + clause.targetStackDepth`, pushes the catch values, and sets
  `frame.pc = clause.targetPC` (`interpreter.go:244-249`). No rollback: locals stay put,
  the value stack is simply unwound to the target depth.
- Cross-frame propagation is a Go `panic(*thrownException)` that each `callWithUnwind`
  re-panics until a frame whose `exceptionTable` matches (`canRestore` gate on frame count,
  `interpreter.go:153-166,254+`).

Two things transfer directly: the **PC-range table** structure (`startPC/endPC/targetPC/
targetStackDepth`) and the innermost-first search. The one wazevo-specific complication:
the interpreter's locals and operand stack are already in memory (the `[]uint64` value
stack), so it needs no register recovery; wazevo's live values may be in registers that a
native unwind clobbers (§4.1).

### 1.5 Serialization (`engine_cache.go`)

`serializeCompiledModule` writes, after the code and the source map, the try-table info:
count, then per table `NumLocals` (4B) + `ReuseLocals` (1B) + clause count (4B) + per clause
`Kind` (1B) + `TagIndex` (4B) (`engine_cache.go:193-209`); `deserialize` mirrors it
(`322-356`) and treats a missing try-table block as a **stale cache** to force recompile
(`324-325`). Note the model already serializes a parallel **source map**
(`executableOffsets`/`wasmBinaryOffsets`) as **executable-relative** offsets that are
rebased to absolute at load (`engine.go:466-476`, `engine_cache.go:180-189` / `298-321`).
This is the exact shape the new side table serializes in.

### 1.6 The existing native-stack unwinder (used today for stack traces)

`unwindStack(sp, fp, top, out)` (`isa_arm64.go:16`, `isa_amd64.go:16`) walks one contiguous
wazevo stack segment and appends return addresses. It is already used by the
`stackIterator` (`call_engine.go:788-813`) and the panic/abort path
(`call_engine.go:300-313`). Two ISA-specific facts drive the whole design:

- **amd64** (`backend/isa/amd64/stack.go:34-72`): frames are chained by a standard **RBP
  frame-pointer chain** — each frame stores `[Caller_RBP, ReturnAddress]`. Walking yields,
  for free, each frame's base RBP. RBP values are **absolute** stack addresses, so a clone
  must rewrite them (`AdjustClonedStack`, `stack.go:91-139`).
- **arm64** (`backend/isa/arm64/unwind_stack.go:22-74`): **no** frame pointer — each frame
  begins with an in-stack `frame_size` word; walking advances `SP += frame_size + 16 +
  8 + size_of_arg_ret`. Saved SPs are **relative to the current SP**, which is why
  `adjustClonedStack` is a no-op on arm64 (`isa_arm64.go:28-32`): after a clone the layout
  shifts wholesale but relative distances are preserved.

Crucially, **wazevo frames are fixed-size** (the prologue emits a compile-time constant
frame size; wasm has no `alloca`). So a function's SP is invariant across its entire body,
and unwinding to a frame recovers exactly the SP/FP the frame would have at *any* point in
its body — including a landing pad. This is what makes SP/FP recovery at throw time exact
without capturing anything at enter (§4.2).

---

## 2. Cost of the current model (why the fast path is expensive)

Per dynamic `try_table` **enter** with catch clauses (the common C++ path, taken whether or
not anything throws):

| Cost | Source |
|---|---|
| Go exit + re-entry (2 mode switches) | `ExitCodeTryTableEnter` trampoline |
| Full stack clone, `make([]byte, len(stack)+16)` + `copy` (>=10KB) | `cloneStack` `call_engine.go:758-786` |
| `savedRegisters` copy by value (1KB) | `tryHandler{savedRegisters: ...}` `:633` |
| `adjustClonedStack` walk (amd64 rewrites the RBP chain) | `:618` |
| Heap alloc of the locals save area (unless `ReuseLocals`) | `:622-626` |
| `append` to `tryHandlers` (may realloc) | `:628` |

Per **leave** (and per branch exiting a catching frame): another full Go round trip.
Per **`local.set` in a try body**: a save-area-ptr load + store, no CSE (**C11**).

None of this is needed when no exception is thrown, which is the overwhelmingly common case.

---

## 3. Design: PC-range exception side table

### 3.1 The compile-time table

Give each compiled module a per-function **exception side table** keyed by native
return-address ranges — the compiled analogue of the interpreter's `exceptionTable` and a
sibling of the existing source map.

Per `try_table`-with-catch, one entry:

```
ehEntry {
    startOffset  uint32   // executable offset (function-relative) of the first
                          // instruction of the try body
    endOffset    uint32   // executable offset one past the last instruction whose
                          // exceptions this try catches (the leave point)
    landingPad   uint32   // executable offset of the landing-pad block (the catch
                          // dispatch), function-relative
    depth        uint16   // lexical nesting depth (for innermost-first ordering)
    clauses      []CatchClauseInstance   // as today: {Kind, TagIndex}
    // reconstruction metadata (see 4.1):
    localSlots   []localSlot   // catch-live locals -> frame slot offset
    floorSlots   []floorSlot   // operand-below-try values -> frame slot offset + SSA type
}
```

Offsets are **executable-relative** (rebased to absolute at load, exactly like
`sourceMap.executableOffsets`, `engine.go:398-399`). The runtime holds them **sorted per
function** so a return address can be binary-searched (like `functionIndexOf`,
`engine.go:960-971`). Because offsets are code-relative and code is never relocated (only
the Go-managed *data* stack is), the table is immune to stack growth (§4.2).

**Recording the offsets.** The backend already tracks each SSA block's executable position
in `labelPosition.binaryOffset` (`backend/isa/arm64/machine.go:129-138`, amd64 equivalent)
and already exposes `AddSourceOffsetInfo(executableOffset, sourceOffset)` /
`SourceOffsetInfo()` (`backend/compiler.go:93-97,343-353`). The new table reuses this
machinery: at lowering, tag the body's first instruction, the leave point, and the
landing-pad block; after codegen, read their `binaryOffset`s to fill `startOffset`,
`endOffset`, `landingPad`. (The source map is currently gated on `module.DWARFLines != nil`
— the EH table must be produced **unconditionally**, so this is a small dedicated pass, not
a reuse of the DWARF gate.)

### 3.2 The non-throwing path (the whole point)

Enter and leave emit **no code at all**. There is no trampoline call, no `tryHandlers`
push/pop, no clone, no register save, no `caughtExceptionClauseIdx` load, no `br_table`.
The try body is compiled as a plain block; the landing-pad block is compiled and left
**unreachable from normal control flow** (reached only by the throw path in §3.3). The only
residual body cost is the memory homes of §4.1 (a subset of C11), which for idiomatic C++
EH is a handful of stores and, for an empty operand floor, exactly the `local.set` mirrors.

### 3.3 The throwing path (rare; may stay a Go exit)

`throw`/`throw_ref` remain a Go exit — throwing is rare, so its cost is irrelevant and
keeping it in Go preserves the existing exnref/params delivery. `ExitCodeThrow` changes
from "search `tryHandlers`" to "walk the native stack against the side table":

1. Read the exnref (as today, `call_engine.go:589-592`); keep it GC-rooted in
   `pendingException`.
2. Walk the current segment with `unwindStack`, but in a variant that yields, per frame,
   `(returnAddress, frameSP, frameFP)` rather than just the return address. amd64 gets the
   frame base from the RBP chain directly; arm64 accumulates SP as it skips frame sizes.
   (This is a additive extension of the existing walkers, §4.2.)
3. For each frame outermost-in-progress (i.e., as we ascend from the throw site): find the
   `compiledModule` (`compiledModuleOfAddr`, `engine.go:714-731`) and function
   (`functionIndexOf`) for `returnAddress`; binary-search that function's `ehEntry`s for all
   ranges containing `returnAddress`, innermost first (`depth` desc / smallest range);
   match clauses against `exn.Tag` in the *frame's* module (mirrors today's "use the
   handler's module for tag identity", `call_engine.go:670-679`).
4. On the first match: set `execCtx.exceptionPtr` / `exceptionParamsPtr` (as today,
   `:596-599`), stash the matched clause index and the landing pad's expected reconstruction
   in execCtx, then **transfer control**: `afterGoFunctionCallEntrypoint(landingPadAddr,
   execCtx, frameSP, frameFP)`. This jumps straight into the catching frame's landing pad
   with the correct SP/FP, discarding all intervening callee frames in one step. No stack
   is cloned or restored; the catching frame's memory is already intact (it lives at higher
   addresses than the abandoned callees).
5. If no frame in the segment matches: `panic(ErrRuntimeUncaughtException)` exactly as today
   (`:593-594`) — see §4.3 for host-boundary semantics.

The landing-pad block does what the current handler blocks do (`reloadAfterCall`,
`loadExceptionParams`, `loadExnRef`, jump to the wasm target), plus it reloads the §4.1
memory homes and threads them to the target block as block params.

---

## 4. The hard problems (with concrete mechanisms)

### 4.1 Landing-pad live state — what may it assume, and where do the values come from?

**What is live into a landing pad?** At a catch target, the wasm operand stack is unwound to
the try's floor and the catch values (exception payload and/or exnref) are pushed. So the
landing pad's live-in set is exactly four categories:

1. **Exception payload** (`catch`/`catch_ref`) — already from memory
   (`exceptionParamsPtr`, `lower.go:4740-4780`).
2. **exnref** (`catch_ref`/`catch_all_ref`) — already from memory (`exceptionPtr`,
   `lower.go:4784-4791`).
3. **Locals** read after the catch — **mutable** in the body, so they need **throw-time**
   values from a memory home. This is what the save area provides today.
4. **Operand-stack values that existed *below* the `try_table` at entry** and are consumed
   by the catch target's continuation — **immutable** during the body (nothing in the body
   can reach beneath its own operand floor), so a **single** memory snapshot suffices. For
   idiomatic C++ EH (a `try` at statement level with an empty operand floor) this set is
   **empty**.

**What can the landing pad assume is in registers? Nothing.** The throw path jumps directly
to the landing pad, skipping every intervening callee's epilogue, so callee-saved registers
hold the deepest throwing frame's values, not the catching frame's. Today this is masked by
restoring `savedRegisters` to the enter-time file (which is why categories 3 and 4 are
"free" today — cat 4 comes back verbatim, cat 3 is then overridden by the save area). The
side table has no register restore, so **categories 3 and 4 must have explicit memory
homes**:

- **Category 3 (locals):** replace the heap save area with **fixed frame slots in the
  catching function's own frame**. `local.set`/`local.tee` inside a try mirror to the frame
  slot (a plain stack store — no `execCtx.localsSaveAreaPtr` load, no heap). The landing pad
  reloads from the frame slot. This is strictly cheaper than today's C11 (no pointer load,
  no heap) and survives the unwind because the catching frame's memory is untouched by the
  abandoned callees. Restrict the mirrored set to **catch-live** locals (locals some
  reachable catch actually reads) to shrink it further.
- **Category 4 (operand floor):** the frontend spills those values to frame slots **once at
  try entry** and the landing pad reloads them. Because they are immutable across the body,
  one store at entry is correct. This is the only residual "enter" cost, and it is **zero
  when the operand floor is empty** (the common case).

**Crucially, this is a frontend-only change — no register-allocator surgery.** The memory
homes are materialized as ordinary `store`/`load` SSA instructions; the landing pad passes
the reloaded values to the target block as ordinary block params. The target block already
receives the same values from its normal-path predecessors as block params, and since the
stored bits equal the live value, the two predecessors agree — SSA stays valid and regalloc
treats everything as ordinary loads/stores. We deliberately **avoid** the general
"invoke/landingpad with a full-clobber abnormal edge" approach (which *would* need regalloc
changes) by exploiting that only two, frontend-computable, categories of value need
recovery.

**The single correctness-critical subtlety** (risk #1, §7): the frontend must compute the
category-3 and category-4 sets *exactly*. Too small -> the handler reads stale/garbage
values (silent corruption, not a crash); too large -> only a mild slowdown. The category-4
set is the operand-stack contents below the `try` at entry that a catch target consumes; the
in-tree oracle for "did we get it right" is the interpreter, which records exactly
`targetStackDepth` per clause (`interpreter.go`), plus the existing
`eh_locals_*` regression corpus (§6).

### 4.2 SP/FP recovery and stack growth

- **No SP is captured at enter** — the design captures nothing at enter. At throw, SP/FP for
  the catching frame come from the native unwind of the **current** stack. Because frames
  are fixed-size, that SP/FP is exactly what the landing pad needs anywhere in the body
  (§1.6).
- **Stack growth / relocation.** Between a (notional) enter and the throw, the Go-managed
  data stack may have been **grown and relocated** (`growStack`/`cloneStack`,
  `call_engine.go:744-786`). The side table is code-relative, so it is unaffected. The
  unwind reads the **current** (post-grow) stack: on amd64 the RBP chain was already
  rewritten by `AdjustClonedStack` at grow time, so current RBPs are valid; on arm64 saved
  SPs are relative and the walk is inherently relocation-safe. A design that captured an
  absolute SP at enter (the current model, and the naive "middle" model) would read a
  **stale** SP after a grow — the side table sidesteps this entirely by capturing nothing.
- **Transfer primitive.** Reuse `afterGoFunctionCallEntrypoint(landingPadAddr, execCtx,
  frameSP, frameFP)`. It already restores `savedRegisters` and jumps with a chosen SP/FP;
  the restored registers are harmless because the landing pad reloads everything it needs
  from memory, and the catching frame's own callee-saved registers are restored correctly
  by *its* epilogue from *its* (intact) frame when it eventually returns.

The unwinder extension is the one piece of new platform-specific code: `unwindStack` must
optionally return frame bases, not just return addresses. Both walkers already compute the
per-frame boundary they need (amd64: `callerRBP`; arm64: the running `i`/SP), so this is
additive and testable against the existing stack-trace tests.

### 4.3 Nesting, throw_ref/exnref, catch_ref, cross-function, host frames, uncaught

- **Nested `try_table`s in one function.** Multiple overlapping `ehEntry` ranges; the throw
  search takes them **innermost-first** (`depth`), matching the interpreter's backward scan.
  If the innermost's clauses do not match, fall through to the next-outer range in the same
  frame, then continue unwinding. This removes today's `ReuseLocals`/nested-save-area
  bookkeeping (`TryTableInfo.ReuseLocals`, `call_engine.go:621-626,711-722`) — nesting is
  just overlapping ranges plus per-function frame slots.
- **`throw_ref` / rethrow.** This dialect has no `rethrow`; `throw_ref exnref` is it, already
  lowered via the same throw trampoline (`lower.go:3531-3547`). Unchanged.
- **exnref values.** The exnref is an `i64` pointer to the `*wasm.Exception`, delivered from
  `execCtx.exceptionPtr` by the (still-Go) throw path; it can be stored in locals, passed
  around, and re-thrown. Keep the `pendingException` GC root (`call_engine.go:577,689`) so
  the object stays alive across the unwind. `catch_ref`/`catch_all_ref` load it in the
  landing pad exactly as today (`loadExnRef`).
- **Cross-function propagation (same segment).** A throw in `C` where `A->B->C` are all
  compiled walks `C->B->A` in one segment and matches each frame's ranges — this is the
  primary generalization the unwinder buys us, replacing the explicit `tryHandlers` stack.
- **Host (Go) frames.** When wasm calls an imported host function, execution **exits to Go**
  (`ExitCodeCallGoFunction`, `call_engine.go:385+`); if that host re-enters wasm it runs on a
  **new** `callEngine.callWithStack` with its **own** stack segment. The native unwinder
  physically cannot walk past a Go frame (it is not in the wazevo stack buffer), so a throw
  is bounded by its segment. If uncaught in the segment it `panic`s
  `ErrRuntimeUncaughtException`, which the segment's `callWithStack` `defer/recover` converts
  to a Go **error** at the host boundary (`call_engine.go:280-331`) — identical to today's
  per-`callEngine` `tryHandlers` behavior. **The design preserves current host-boundary
  semantics exactly** (exceptions degrade to errors across host calls; they do not silently
  cross them).
- **Uncaught -> trap.** Same as today: `panic(ErrRuntimeUncaughtException)` -> error.

### 4.4 Snapshot / checkpoint and listener interplay

- **Snapshot.** The current design *reuses* `cloneStack`/`savedRegisters` for both snapshot
  and try. The side table decouples them: `try` no longer clones; `experimental.Snapshot`
  keeps its own `cloneStack` (`call_engine.go:856-905`) untouched. This is a net
  **simplification and a latent-bug fix**: today `tryHandlers` is `callEngine` state that a
  snapshot restore does **not** save/restore, so a snapshot taken across a `try` boundary can
  desynchronize the handler stack. With the side table there is **no** such runtime state —
  the "handler stack" is implicit in the native stack, which snapshot already clones — so
  snapshot/restore across `try` bodies becomes correct by construction.
- **Listeners.** Listeners use `stackIterator`/`unwindStack` (`call_engine.go:788-813`) and
  fire at call boundaries; the abort path already calls `lsn.Abort` per frame on uncaught
  panic (`call_engine.go:296-319`). Behavior on a **caught** exception is preserved: frames
  between throw and catch are abandoned without firing their after-listeners, exactly as
  today's `doHandleException` discards them. (Firing after-listeners during an EH unwind is
  out of scope and matches current behavior.)

### 4.5 Serialization and determinism

Replace the serialized `tryTableInfo` block (`engine_cache.go:193-209,322-356`) with the
side table: per module, a count, then per function a count and per `ehEntry`
`{startOffset, endOffset, landingPad, depth, clauses[], localSlots[], floorSlots[]}`, all
offsets **executable-relative** and rebased at load (same pattern as the source map). It is
deterministic because every field derives from deterministic compilation (block offsets from
codegen, slot assignments from the frontend). Bump the cache format so old entries recompile
— the existing "missing try-table block => staleCache" path already gives us this
(`engine_cache.go:324-325`); extend it to "old try-table shape => staleCache". `CatchClauseInstance`
(`wazevoapi/try_table.go`) is reused; `TryTableInfo` (with `NumLocals`/`ReuseLocals`) is
retired in favor of `ehEntry`.

---

## 5. Expected win and benchmarking

**What exists.** The only EH tests are correctness/conformance: the exception-handling
spectest dialect (`spectest/exception-handling/spec_test.go` runs `throw`, `throw_ref`,
`tag`, `try_table` on both compiler and interpreter), `eh_hammer_test.go` (parallel-compile
stress, always throws), and `exceptions_test.go` (`eh_pdfium`, `eh_cross_callnative`,
`eh_locals_*`, `eh_br_*`). **There is no benchmark of the non-throwing fast path** — grep of
`benchmarks/` finds none.

**Benchmark to add** (`benchmarks/` or `internal/integration_test/engine`): a C++-shaped
module that **enters `try_table` in a hot loop without ever throwing**, e.g.:

```wat
(func (export "hot") (param $n i32) (result i32)
  (local $i i32) (local $acc i32)
  (loop $L
    (block $catch
      (try_table (catch_all $catch)
        ;; trivial body that never throws; call a leaf fn to keep the catch "live"
        (local.set $acc (i32.add (local.get $acc) (call $leaf (local.get $i)))))
      (br $done))
    (local.set $i (i32.add (local.get $i) (i32.const 1)))
    (br_if $L (i32.lt_u (local.get $i) (local.get $n))))
  (local.get $acc))
```

Measure ns/op for large `$n`, plus a variant with a non-empty operand floor and a nested
variant, comparing:

- current model (enter Go-exit + clone + 1KB + leave Go-exit per iteration),
- side table (zero enter/leave; only §4.1 stores).

**Expected magnitude.** Each iteration currently pays two Go mode-switch round trips plus a
>=10KB `copy` plus a 1KB register copy (and, first time, a heap alloc); the side table pays
**zero** for enter/leave, leaving only the loop's real work plus, at most, a couple of frame
stores. For an EH-in-hot-loop microbenchmark this is expected to be a **large multiple**
(order-of-magnitude class) improvement; for pdfium/Emscripten-style whole-program workloads
the win tracks how enter-heavy the code is. Throwing-path latency is unchanged-to-slightly-
better (no clone to allocate/copy; one unwinding transfer instead of a stack restore).

---

## 6. Migration and testing

### Behind existing tests (no new oracle needed)

- **Spectest EH dialect** (`throw`/`throw_ref`/`tag`/`try_table`) on the compiler — the
  conformance backstop for every phase.
- **Interpreter differential** — the interpreter's table model (`targetPC`,
  `targetStackDepth`, backward search) is the exact reference for the compiler's table and
  for the category-4 floor set; run both engines over the same corpus.
- **`exceptions_test.go` corpus** — `eh_locals_corrupted`, `eh_locals_nested_catch`,
  `eh_locals_cross_func`, `eh_locals_nested_nocatch` pin the save-area/locals contract that
  §4.1 must preserve; `eh_cross_callnative`, `eh_pdfium`, `eh_br_*`, `eh_catch_outside`,
  `eh_throw_ref_null` pin cross-frame, host-boundary, branch-exit, and null-exnref
  semantics.
- **`eh_hammer_test.go`** — parallel-compile determinism of table IDs/offsets.

### New tests required

- Throw **across host frames** (wasm->host->wasm) asserting the segment boundary degrades to
  an error, both directions of the boundary.
- Throw **after a stack grow** between enter and throw (force `growStack` inside a deep try
  body, then throw) — validates code-relative ranges + current-stack SP/FP recovery on both
  ISAs.
- **Deep nesting** and multi-frame innermost-first dispatch; overlapping ranges in one
  function.
- **Non-empty operand floor** into a catch target (category-4 reconstruction).
- **Snapshot across a try body** (the latent-bug case §4.4).
- amd64 **and** arm64 in CI (the unwinder extension and transfer are the platform-specific
  surface).

### Phase breakdown (Sonnet MVP -> Opus finalize)

- **Phase 0 — table plumbing (low risk).** Build/serialize/rebase the `ehEntry` table
  (offsets from `labelPosition.binaryOffset`), keep the *current* runtime. Verify offsets
  against the interpreter's PC ranges. No behavior change yet.
- **Phase 1 — MVP: kill the Go round trips and the clone (low-med risk, Sonnet).** Keep the
  existing catch semantics (register restore + a locals memory home) but drive them from the
  side table: on throw, unwind + match against ranges, recover SP/FP, transfer to the landing
  pad; **remove** `ExitCodeTryTableEnter`/`Leave`, `cloneStack`-for-try, the `tryHandlers`
  stack, and the heap save area (move locals to frame slots). This already deletes the
  dominant costs (Go exits, >=10KB clone, heap alloc) and makes enter/leave nearly free. If
  register recovery of category 4 is deferred, keep a *small* register save only for the
  (usually empty) operand floor as a safety net.
- **Phase 2 — finalize: truly zero-cost + C11 removal (med risk, Opus).** Implement the full
  §4.1 frontend reconstruction (category-3 frame-slot mirror restricted to catch-live
  locals; category-4 spill-at-entry) and **drop the register save entirely**. Enter/leave
  emit zero instructions; C11 shrinks to catch-live frame-slot stores. Add the fast-path
  benchmark and the new tests above. Tighten the category-3/4 sets and prove them against the
  interpreter oracle.
- **Phase 3 — cleanup.** Retire `TryTableInfo.ReuseLocals`, `localsSaveAreaPtr`, the enter/
  leave trampolines and exit codes; document the new cache format.

---

## 7. Risk ranking and feasibility verdict

**Three riskiest parts**

1. **Live-value reconstruction correctness (§4.1)** — the frontend must compute the
   catch-live locals (cat 3) and the operand-floor (cat 4) sets exactly; an under-set is
   *silent* value corruption on the caught path. Mitigation: the interpreter is a precise
   in-tree oracle (`targetStackDepth`), the `eh_locals_*` corpus already probes this, and
   the common case (empty floor, few catch-live locals) is simple. This is the reason Phase 1
   keeps register restore as a safety net.
2. **SP/FP recovery + native transfer across ISAs (§4.2)** — extending both unwinders to
   report frame bases and jumping to a landing pad with recovered SP/FP, correct under stack
   growth. amd64 (RBP chain, absolute) and arm64 (frame-size walk, relative) differ; this is
   the platform-specific asm-adjacent surface. Mitigation: the walkers already compute the
   needed boundary; test on both arches with the stack-grow case.
3. **Semantic-preservation at boundaries (§4.3-4.4)** — host-frame degradation, uncaught->
   trap, snapshot/listener interplay, nested/overlapping ranges. Lower risk (mostly
   preserved-by-construction) but broad; covered by the existing corpus plus the new
   boundary tests.

**Is anything fundamentally harder than the scan assumed?** Partially. The scan framed C1 as
"PC-range side table + unwinder (already exists)". The table, the unwinder, the
address->function map, and the transfer primitive **all already exist or are trivial
extensions** — that part is as easy as assumed. What the scan under-weighted is **landing-pad
live-state reconstruction**: the current fast path is "expensive" precisely because it buys
correctness cheaply via a full register+stack restore, and removing that restore means the
handler's live-ins must be reconstructed from memory. The good news, established in §4.1, is
that this needs **no register-allocator changes** — only two frontend-computable value
categories need memory homes, and both lower to ordinary stores/loads. A fully general
solution (LLVM-style invoke/landingpad with a clobber-all abnormal edge, letting regalloc
spill arbitrary live-ins) *would* require regalloc work and is explicitly **not** needed
here.

**Alternatives sized**

- **Full side table (recommended target).** Zero-cost enter/leave, C11 reduced to catch-live
  frame stores, snapshot bug fixed, cache simplified. Cost: §4.1 frontend work + unwinder
  extension. This is the right end state.
- **Middle design (native handler stack, no Go exits).** Keep enter/leave but implement them
  as a few native instructions pushing/popping a handler record in a per-`callEngine` array
  (store **relative** SP/FP so it survives stack growth), keep a native register
  save/restore, keep a locals memory home. This removes the Go round trips, the >=10KB clone,
  and the heap alloc **without any live-value analysis or regalloc reasoning** — but enter
  still costs a register-subset copy and leave a pointer bump, so it is not zero-cost.
  ~90% of the win for ~30% of the risk.
- **Defer.** Only if EH is not on a measured hot path for target workloads.

**Recommendation: pursue the full side table, staged.** Ship **Phase 1 (the middle design's
substance: delete the Go exits, the clone, the heap alloc; drive catch from the side table
with a register-restore safety net)** first — it is low-risk and captures the dominant cost.
Then complete **Phase 2** to reach truly zero-cost enter/leave and remove C11. This ordering
lets a Sonnet-level pass land the big, certain win behind the entire existing EH test corpus,
and reserves the one subtle, correctness-critical piece (live-value reconstruction) for an
Opus-level finalize with the interpreter as oracle.
