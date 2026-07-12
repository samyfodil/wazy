# P3: a backend ABI for zero-cost `try_table` enter (wazevo)

Feasibility spike + implementation plan for **phase P3** of the exception-handling
redesign (C1). Companion to `docs/design/eh-side-table.md`; read that first, especially
§3.3, §4.1's `CORRECTION` paragraph, and §7. This document is a design/reverse-engineering
artifact only — **no P3 code is implemented here**. The findings below were gathered by
reading the shipped C1 P0/P1/P2a code and by throwaway experiments (a no-EH floor
benchmark and a `PrintFinalizedMachineCode` dump), all reverted; the tree is clean.

---

## OUTCOME (P3.0 shipped; P3.1+ BLOCKED — read before implementing)

**P3.0 shipped** (fixed per-frame slots for execCtx/moduleCtx, gated on
try_table-with-catch functions; nothing reads them yet). The P3.2 *regalloc* crux was
validated GO: regalloc keeps and correctly allocates an abnormal-edge landing pad, and a
landing pad with a genuinely empty register live-in is entered correctly by a direct throw
transfer on both ISAs.

**But P3.1 (drop the register snapshot) is BLOCKED, and the block is fundamental.** An
attempt to drop the snapshot for the empty-operand-floor case — with execCtx/moduleCtx
memory-homed via the P3.0 slots — was disproven by a **real C++ workload**
(`experimental.TestCppExceptions/test_catch_specific`, `cpp_exceptions.wasm` funcIdx 2: a
`catch_all_ref` handler whose continuation calls two more functions before returning). Even
with execCtx, moduleCtx, cat-3 locals, and an *empty* cat-4 floor all accounted for, the
handler continuation still reads **some further category of callee-saved-register-resident
state** that only the full snapshot restore provides. A/B confirmed reproducibly: forcing
unconditional snapshot capture fixes it; gating on `FloorSize==0` does not. The extra
category was not fully identified.

This is the **third** time a careful enumeration of "what a landing pad needs" has been
wrong: §4.1 missed `moduleCtx`; the P3 spike (below) missed this category; and the resume
point's true callee-saved live-in set has resisted three analyses. The lesson: the
enter-continuation resume point inherits the *entire* callee-saved register file by
construction (the register allocator may keep any cross-`try` value there), so enumerating
a safe subset to memory-home is not a frontend-tractable problem. Dropping the snapshot
safely requires a **general abnormal-edge liveness model in the register allocator** (the
"clobber-all invoke/landingpad edge" the design explicitly ruled out of scope), not the
incremental memory-homing this document proposed.

**Conclusion: the residual 1 KB register snapshot per enter is the practical floor.**
C1 is effectively complete at P3.0. The remaining win (deleting the two Go exits per enter,
~90× on the EH-fast-path microbenchmark) is real but is gated behind allocator surgery whose
risk/effort is disproportionate to the benefit for current workloads. A secondary blocker
found along the way: routing execCtx/moduleCtx reads through `ssa.Builder.MustFindValue` on
an *unsealed* block inserts a spurious block param in **every** function (EH or not), because
`Seal`'s `unknownValues` path adds a param unconditionally rather than taking `findValue`'s
"same value from every predecessor" shortcut — any revival must solve that too. Everything
below is the original spike; it is preserved for context but its "memory-home the four
categories" premise is **superseded by this outcome**.

---

## Executive summary

**Where we are.** C1 P0/P1/P2a are shipped. The *throw* path already unwinds the native
stack against a per-function PC-range side table (`compiledModule.ehTables`,
`wazevoapi/eh_table.go`) and transfers control with a fresh SP/FP — no stack clone.
But **enter is still a Go exit** (`ExitCodeTryTableEnter`, `call_engine.go:697-784`) that
per dynamic entry (a) pushes an `activeCatchScope`, (b) copies the **1 KB**
`execCtx.savedRegisters` snapshot (`[64][2]uint64`, `call_engine.go:776`), and (c) acquires
a locals mirror buffer. **Leave is a second Go exit** (`ExitCodeTryTableLeave`,
`call_engine.go:785-794`). The throw path resumes the catching frame at its **enter-
continuation** (the reload+`br_table` after the enter trampoline call), not at a raw landing
pad, and restores the register snapshot to make that resume point correct.

**Q1 — how is execCtx carried?** Verdict **(b): an ordinary vreg**. `execCtxPtrValue` /
`moduleCtxPtrValue` are entry-block SSA params (`frontend.go:424-425`), passed in
`rax`/`rbx` (amd64) or `x0`/`x1` (arm64), and thereafter placed by regalloc anywhere —
callee-saved reg or spill slot. **No register is reserved** for either (amd64
`AllocatableRegisters` is every GPR but `rsp`/`rbp`, `abi.go:17-48`; arm64 reserves only
`x18`/`x28`/`x27=tmp`, `abi.go:18-37`). **There is no backend-guaranteed fixed location
where execCtx is recoverable at an arbitrary PC.** In practice regalloc *does* spill execCtx
to a per-function slot in call-heavy functions (measured: `[rsp+0]` in the benchmark
function, reloaded at the enter-continuation), but the offset is regalloc-chosen and not
guaranteed to exist. Mid-body execCtx access is just an ordinary vreg use (e.g. every
trampoline-address load `movq (%rsp),%rdi; callq *1200(%rdi)`).

**Q2 — is there a backend-controlled fixed frame slot?** Not today, but one is **cheap to
add**. Frames are fixed-size; spill slots are SP-relative and assigned by
`getVRegSpillSlotOffsetFromSP` growing from offset 0 (amd64 `machine.go:2344-2352`, arm64
`machine_regalloc.go:294/318`). Reserving the first one or two spill words for
execCtx/moduleCtx, storing them in the prologue (execCtx/moduleCtx are in `rax`/`rbx`,
`x0`/`x1` at entry — the prologue already reads execCtx there for the bounds check), and
reloading in landing pads is a small, mechanical change that reuses existing machinery and
is **relocation-safe** (SP-relative). **This is the P3 enabling primitive.**

**Q3 — the abnormal edge.** The shipped model resumes at the **enter-continuation
(option a)** — a normal SSA edge — so no abnormal edge exists today. **Dropping only the
snapshot** does not need one: keep resuming at the enter-continuation and give
moduleCtx + the operand-floor their own memory homes. **Zero-cost enter** *does* need the
§3.3 direct-landing-pad jump, i.e. a landing-pad block reached by an external (non-CFG)
transfer. This is a *tamed* abnormal edge: if the landing pad reloads **all** its live-ins
from memory at its top (execCtx/moduleCtx from fixed slots, locals from frame slots,
exception params from execCtx), its **register live-in set is empty**, so regalloc needs no
clobber-all liveness model — only a guarantee the block is kept reachable/allocated. That is
far less than LLVM-style invoke/landingpad.

**Q4 — can the enter Go exit go away?** Yes. The two things the scope carries that a bare
throw-walk can't recompute are `moduleInstance` and `resumePC`. `moduleInstance` is
recoverable from any frame's moduleCtx via `moduleInstanceFromOpaquePtr` (opaque offset 0 is
`*wasm.ModuleInstance`, `module_engine.go:359-360,158`) — so the fixed moduleCtx slot doubles
as the per-frame module source. `resumePC` disappears: resume at the side table's already-
recorded `LandingPad` (`EhClause.LandingPad`, `eh_table.go:15-19`, currently a dead-code
marker) instead of the enter-continuation. With both recovered from the side table + fixed
slots, **enter and leave emit zero instructions** and `activeCatchScopes` is deleted.

**Q5 — two ISAs + stack growth.** Already handled by the shipped throw path and unchanged by
P3: the throw does a **fresh** walk of the current stack (`unwindStackForThrow`), so it is
immune to a grow/relocate between enter and throw; fixed **frame slots** are SP-relative
(relocation-safe); `resumePC`/`LandingPad` are code addresses in the never-relocated
executable. No mechanism proposed here captures an absolute address at enter (P3 captures
*nothing* at enter). amd64 recovers FP from the RBP chain and computes `SP=FP-FrameSize`
(`isa_amd64.go:44-49`); arm64 recovers SP directly from the frame-size chain
(`isa_arm64.go:40-42`).

**Achievable win (measured, amd64, this box).** A `try_table` entered every iteration of a
hot loop that never throws costs **~86 ns/iter**; the identical loop with the `try_table`
removed costs **~1 ns/iter**. So EH machinery is **~99%** of the fast-path time and truly
zero-cost enter/leave is a **~90× speedup on this microbenchmark** (order-of-magnitude class,
matching `eh-side-table.md` §5's prediction). Whole-program wins track how enter-heavy the
workload is.

**Go / no-go.** **GO, staged, Opus-owned.** The win is large and real, every supporting
primitive (side table, unwinder-with-frame-bases, address→function map, SP/FP transfer,
per-frame module recovery) already exists, and the one genuinely new piece — a fixed-slot
frame ABI + a self-sufficient landing pad — is well-scoped. **The cheap partial (drop only
the snapshot) is NOT worth shipping alone**: the two Go exits dominate the 86 ns, so removing
just the ~1 KB copy captures a low-single-digit-percent slice. The value is in deleting the
Go exits, which requires the full model. **Recommended design: fixed frame slot** (not a
reserved register — see Q1/§Design).

---

## Q1 — How is `execCtxPtrValue` physically carried?

**It is an ordinary vreg (answer (b)).** Both context pointers enter as SSA params of the
entry block:

- `frontend.go:424-425`:
  `c.execCtxPtrValue = entryBlock.AddParam(builder, executionContextPtrTyp)` and the same for
  `moduleCtxPtrValue`. They are whole-function-live values used pervasively — e.g. every
  trampoline-address load and trap exit dereferences execCtx (`lower.go:527,592,1189,3400,
  3505,…`), and memory base / globals load through moduleCtx (`lower.go:1155,1165,568,…`).

**No register is reserved for them.** The allocatable sets contain no carve-out:

- amd64 (`backend/isa/amd64/abi.go:17-48`): `AllocatableRegisters` = every GPR except
  `rsp`/`rbp`; callee-saved = `rdx,r12,r13,r14,r15` (+xmm8-15). Args come in
  `intArgResultRegs = {rax,rbx,rcx,rdi,rsi,r8,r9,r10,r11}` (`abi.go:13`), so **execCtx enters
  in `rax`, moduleCtx in `rbx`** (confirmed by `abi_entry_amd64.s:8-9`).
- arm64 (`backend/isa/arm64/abi.go:18-45`, `reg.go`): reserved regs are only `x18` (macOS),
  `x28` (Go g), `x27` (tmp). Args in `x0..x7`; **execCtx in `x0`, moduleCtx in `x1`**
  (`abi_entry_arm64.s:11-12`, and the stack-grow prologue comment "x0 … always points to the
  execution context whenever the native code is entered from Go", `machine_pro_epi_logue.go:
  349`).

**Is execCtx recoverable at any PC from a fixed location? No.** After entry, regalloc treats
execCtx like any vreg. The decisive evidence is the finalized amd64 code for the benchmark's
`hot` function (`PrintFinalizedMachineCode`, reverted):

```
; prologue — regalloc happened to spill execCtx and moduleCtx to slots:
mov.q %rax, (%rsp)      ; execCtx  -> [rsp+0]   (regalloc spill slot 0)
mov.q %rbx, 16(%rsp)    ; moduleCtx-> [rsp+16]  (regalloc spill slot)
...
; enter trampoline call site (blk4):
movq (%rsp), %rdi       ; reload execCtx from its slot to pass as arg0
callq *1200(%rdi)       ; execCtx.tryTableEnterTrampolineAddress  (the Go exit)
movq (%rsp), %rax       ; ENTER-CONTINUATION: reload execCtx from [rsp+0]
movq 1232(%rax), %rcx   ; execCtx.caughtExceptionClauseIdx -> br_table
```

So *in this function* execCtx lives at `[rsp+0]` for the whole body and the enter-continuation
reloads it from there — matching the arm64 note `ldr x8,[sp,#16]` in
`abi_entry_arm64.s:39`. **But that offset is regalloc-chosen and not guaranteed.** In a
register-pressured real function regalloc may instead keep execCtx/moduleCtx in a *callee-
saved register* across the try and never spill — which is exactly the case
`call_engine.go:751-772` documents as having crashed a real C++/Emscripten module when the
snapshot was skipped. **Conclusion:** execCtx is a wandering vreg; there is no fixed register
or fixed slot the backend guarantees today. P3 therefore needs to *create* such a fixed
location. Same answer for moduleCtx.

## Q2 — Is there a backend-controlled fixed frame slot? Can we add one?

**Today: no dedicated fixed slot.** Frame layout:

- **arm64** (`machine_pro_epi_logue.go:17-149`): from high→low — args/rets, `size_of_arg_ret`,
  return address, then **clobbered callee-saved regs**, then the **spill-slot region**, then a
  `frame_size` word. The prologue does **not** spill args (execCtx/moduleCtx) to any fixed
  slot; it only saves clobbered callee-saved registers.
- **amd64** (`abi.go:76-90` layout diagram, `machine.go:2401-2422`): `[Caller_RBP,
  ReturnAddress]` at RBP, then clobbered regs (integer via 8-byte `PUSH`, xmm via 16-byte
  slot), then the spill-slot region down to RSP. `FrameSize()` = clobbered bytes +
  `spillSlotSize`.

**Adding a fixed slot is cheap and mechanical.** Spill slots are SP-relative and handed out by
`getVRegSpillSlotOffsetFromSP(id,size)` starting at offset 0 and growing (amd64
`machine.go:2344-2352`; arm64 uses the same helper, `machine_regalloc.go:294,318`). Because
they grow *upward from 0*, **reserving the first one or two words gives a compile-time-constant
offset** (`[SP+0]`, `[SP+8/16]`) regardless of how many other slots the function ends up
needing. The plan:

1. Before regalloc, reserve two fixed words at SP offsets 0/8 (int) for execCtx and moduleCtx
   (bump the base so regalloc's own slots start after them). Expose the offsets via a new
   `backend.Machine` accessor (e.g. `EhCtxSlotOffsets() (execOff, modOff int64)`), analogous
   to how `FrameSize()` is already exposed for the throw transfer (`machine.go:2387-2411`).
2. In `setupPrologue` (both ISAs), emit two stores of the entry-ABI registers
   (`rax`/`rbx`, `x0`/`x1`) into those slots — two instructions, on the cold prologue path
   only, and only for functions that contain a `try_table` (gate on a per-function flag the
   frontend already knows).
3. Landing pads reload execCtx from `[SP+execOff]` (SP-relative; needs only SP set, which the
   throw transfer already does) and everything else through it.

Room: yes — this adds 16 bytes to the frame of try-containing functions; frames are already
variable-size and 16-byte aligned. **Relocation-safe** because SP-relative (§Q5). There is a
pre-existing convention to piggy-back on: the frontend's category-3 **locals mirror** should
move from today's heap `localsSaveArea` (addressed through `execCtx.localsSaveAreaPtr`,
`call_engine.go:231-233`, `lower.go` save-area stores) to these same fixed frame slots — one
uniform "reserved EH frame region" holding {execCtx, moduleCtx, mirrored locals, operand
floor}. That removes the heap buffer, the pool, and `localsSaveAreaPtr` entirely.

## Q3 — The abnormal edge in regalloc

**Shipped model = option (a), no abnormal edge.** The throw transfer resumes the catching
frame at its **enter-continuation** `resumePC` (`call_engine.go:885-897`,
`activeCatchScope.resumePC` doc `:128-146`), i.e. the compiled reload+`br_table` right after
the enter trampoline call. That block is reached in the SSA CFG by an ordinary edge from the
enter call; regalloc sees nothing unusual. Correctness of the resume comes from restoring the
**register snapshot** (`afterThrowTransferEntrypoint` reloads callee-saved regs from
`execCtx.savedRegisters`, `abi_entry_amd64.s:67-80`, `abi_entry_arm64.s:75-86`) so the
enter-continuation's register world matches enter time.

**Precisely which register reads force the snapshot** (the enumeration `eh-side-table.md`
§4.1's CORRECTION asked for): between arriving at `resumePC` and the handler finishing, the
chain reads, from callee-saved registers:

- **execCtx** — read immediately (`movq 1232(%rax)` = `caughtExceptionClauseIdx`). *Already
  covered by memory*: the enter-continuation reloads it from its spill slot
  (`movq (%rsp),%rax`), so execCtx per se does **not** require the snapshot in functions where
  regalloc spilled it — but that is not guaranteed (Q1).
- **moduleCtx** — every handler block calls `reloadAfterCall`, which loads memory base/length
  and mutable globals *through* moduleCtx (`lower.go:1155,1165`). If regalloc kept moduleCtx in
  a callee-saved reg across the body (not spilled), the abandoned callees clobbered it →
  requires the snapshot. **This is the documented crash cause** (`call_engine.go:754-772`).
- **operand-floor (category 4)** and any **other SSA value the catching function keeps live
  across the try body in a callee-saved register** and consumes after the catch merge. In wasm
  every cross-block value is a local or an operand-stack entry, so this set = {locals (cat 3,
  already memory-homed via the save area), operand-stack values below the try (cat 4)}. For
  idiomatic C++ EH (empty operand floor) cat 4 is empty.

So the complete set that the snapshot masks is exactly **{execCtx, moduleCtx} ∪ cat-4 floor**
(cat-3 locals are already memory). That finite, frontend-/backend-computable set is what makes
the snapshot replaceable.

**What zero-cost enter needs.** Removing the Go exit removes the enter call, hence the enter-
continuation `resumePC` ceases to exist. The throw must then land at the **raw landing pad**
(`EhClause.LandingPad`). That block is reached by an *external* transfer, not a CFG edge — the
§3.3 direct-landing-pad model. This is an abnormal edge, but a **tamed** one: structure the
landing pad to reload **all** live-ins from memory at its top —

- execCtx ← `[SP+execOff]` (fixed slot),
- moduleCtx ← `[SP+modOff]` (fixed slot) → then `reloadAfterCall` through it,
- locals ← fixed frame slots (cat 3),
- operand floor ← fixed frame slots (cat 4),
- exception params/exnref ← execCtx (as today, `lower.go:4740-4791`),

— so its **register live-in set is empty**. Then regalloc needs no clobber-all abnormal-edge
liveness; it only must (i) keep the block allocated/reachable despite having no normal
predecessor and (ii) not assume any register value flows in. Mechanically this can be a block
with a synthetic/pseudo predecessor (or a dedicated "landing pad" block kind that the layout
keeps and regalloc treats as a second entry). **This is the single subtle regalloc-adjacent
task in P3** (risk #1), but it is materially smaller than the general invoke/landingpad the
side-table doc's §7 ruled out.

## Q4 — Can the Go exit on enter be removed independently?

**Yes.** The enter scope carries `{moduleInstance, resumePC, savedRegisters, localsSaveArea}`
(`call_engine.go:123-161`). Take them one by one under a zero-instruction enter:

- **savedRegisters** → eliminated by the fixed-slot reloads of Q2/Q3 (execCtx, moduleCtx,
  cat-4 floor all memory-homed; the landing pad has no register live-ins).
- **localsSaveArea** → the cat-3 locals mirror moves to fixed frame slots (Q2). No heap
  buffer, no pool, no `localsSaveAreaPtr`.
- **moduleInstance** → recovered at throw time from the frame's own moduleCtx fixed slot:
  `moduleInstanceFromOpaquePtr(moduleCtx)` reads `*wasm.ModuleInstance` at opaque offset 0
  (`module_engine.go:359-360`, `ModuleInstanceOffset = 0` at `:158`). This is exactly what the
  fixed moduleCtx slot is for — the throw walk already visits each frame; it now also reads
  `[frameSP+modOff]` to get that frame's module for tag matching (replacing
  `scopes[scopeIdx].moduleInstance`, `call_engine.go:844`).
- **resumePC** → gone. Resume at the side table's `LandingPad` offset (already recorded per
  clause, `eh_table.go:9-20`; today only a dead-code marker per `call_engine.go:870-874`).
  `handleThrow` already computes the frame, function index, matching clause `ci`, and SP/FP;
  it simply transfers to `EhClause.LandingPad` instead of `resumePC`, and no longer sets
  `caughtExceptionClauseIdx` for a `br_table` (each clause has its own landing pad, so the
  matched clause *is* the destination — `eh_table.go:3-8`).

With all four sourced from the side table + fixed slots, **`ExitCodeTryTableEnter` and
`ExitCodeTryTableLeave` are deleted**, `activeCatchScopes` and its lockstep counter go away,
enter/leave emit nothing, and the model becomes the pure side-table design of
`eh-side-table.md` §3. This reconnects directly to §3.3's direct-landing-pad transfer, now
honestly achievable because the fixed slot breaks the "need execCtx to load execCtx"
circularity the P2b CORRECTION hit: the landing pad loads execCtx **SP-relative**, needing no
execCtx in hand.

## Q5 — Two ISAs + stack growth

Everything here is already implemented by the shipped throw path and is **unchanged** by P3;
P3 only adds the fixed-slot stores/reloads, which are themselves relocation-safe.

- **Nothing captured at enter.** P3 captures no SP/FP/PC at enter. At throw, SP/FP come from a
  **fresh** walk of the *current* stack: amd64 gets FP from the RBP chain and computes
  `SP = FP − FrameSize()` (`isa_amd64.go:44-49`, `functionFrameSizes`); arm64 gets SP directly
  from the per-frame `frame_size` chain (`isa_arm64.go:40-42`, `unwind_stack.go`). Because
  wazevo frames are fixed-size (no `alloca`), that SP/FP is exact anywhere in the body,
  including a landing pad (`eh-side-table.md` §1.6).
- **Stack grow/relocate between enter and throw is safe by construction.** The side table is
  code-relative; `LandingPad`/return addresses are addresses in the mmap'd, never-relocated
  executable; SP/FP are read post-grow from the current stack (amd64's RBP chain was already
  rewritten by `AdjustClonedStack` at grow time; arm64's saved SPs are relative,
  `isa_arm64.go:62-66`). **Fixed frame slots are SP-relative**, so they move with the frame and
  are read correctly after a grow. **No absolute address is captured** — flag: any future
  temptation to cache an absolute slot address would break this; keep slot access
  `[SP+const]`.
- **Transfer primitive exists.** `afterThrowTransferEntrypoint(restoreFn, execCtx, sp, fp,
  targetPC)` already sets SP/FP and jumps (`abi_entry_amd64.s:55-87`, `abi_entry_arm64.s:57-90`).
  Under P3 it stops restoring `savedRegisters` (the landing pad reloads from memory instead) —
  simplifying both `.s` files to "set SP/FP, jump to landingPad". The `restoreFn` blob
  (`CompileThrowTransferRegisterRestore`) and the whole `savedRegisters` field become
  removable.

---

## Recommended P3 design: **fixed frame slot** (not reserved register)

**Chosen mechanism: a reserved, backend-controlled, SP-relative frame region** holding
execCtx + moduleCtx (+ the cat-3 locals mirror and cat-4 floor), stored in the prologue of
try-containing functions and reloaded by self-sufficient landing pads.

**Why not a reserved register.** Pinning execCtx to a callee-saved register for the whole
function would make the transfer trivial (just set the register), but:

- amd64 has only **5** callee-saved GPRs (`rdx,r12-r15`); removing one from allocation is real
  pressure on register-heavy code and would tax **all** functions unless done per-function,
  which needs a variable-ABI (call sites must agree) — high complexity for `call_indirect`
  signatures (the very reason `frontend.go:414-419` keeps the ctx params unconditional).
- arm64 has 8 callee-saved GPRs so it is cheaper there, but the asymmetry and the whole-program
  tax still lose to the fixed slot.

The fixed slot costs only **two prologue stores** (cold path) + reloads on the **rare** throw
path, adds **no register pressure**, reuses the spill-slot machinery, and is relocation-safe.
It also uniquely enables per-frame `moduleInstance` recovery (Q4) for free.

### Exact backend/runtime files & functions to change

Backend (the new primitive):

- `backend/machine.go` + `backend/isa/{amd64,arm64}/machine.go`: reserve the EH ctx slots
  (bump the spill-slot base; add `EhCtxSlotOffsets()`), and make `FrameSize()` account for
  them. (`machine.go:2344-2352,2387-2411` amd64; arm64 counterparts.)
- `backend/isa/{amd64,arm64}/machine_pro_epi_logue.go` `setupPrologue`: emit the two ctx
  stores (`rax`/`rbx`, `x0`/`x1` → reserved slots), gated on a per-function "has try_table"
  flag. (amd64 `machine_pro_epi_logue.go`; arm64 `:17-149`.)
- `backend/compiler.go`: the EH-entry offset-recording pass already exists conceptually
  (`AddSourceOffsetInfo`/labelPosition, `:93-97`, and `ehTables` are built from block offsets);
  extend it to tag landing-pad blocks and (already) their offsets.
- `backend/isa/{amd64,arm64}/abi_entry_*.s` + `entrypoint_*.go`: drop the `savedRegisters`
  restore from `afterThrowTransferEntrypoint`; it becomes "set SP/FP, jump to landingPad".
  Retire `CompileThrowTransferRegisterRestore` (`abi_go_call.go`,
  `machine_pro_epi_logue.go:493-506`).

Frontend (self-sufficient landing pads + fixed-slot locals):

- `frontend/lower.go`: (i) stop emitting the enter/leave trampoline calls and the post-enter
  `br_table`; (ii) emit each catch clause's **landing pad** as a block reached only by the
  throw transfer, reloading execCtx/moduleCtx from the fixed slots and calling
  `reloadAfterCall` through the reloaded moduleCtx; (iii) replace `storeLocalToSaveArea` /
  `reloadLocalsFromSaveArea` (heap, via `localsSaveAreaPtr`) with stores/loads to the reserved
  frame slots; (iv) spill the cat-4 operand floor to frame slots at try entry (usually empty).
  A new SSA facility is needed for the frontend to address fixed frame slots (it has none
  today — the P2b blocker); model it as a backend intrinsic load/store keyed to
  `EhCtxSlotOffsets()`.
- The category-3/4 set computation must be **exact** — the interpreter's `targetStackDepth`
  is the in-tree oracle (`eh-side-table.md` §4.1, §6).

Runtime (delete the enter/leave Go path):

- `call_engine.go`: remove `ExitCodeTryTableEnter`/`Leave` cases (`:697-794`),
  `activeCatchScopes` and `activeCatchScope` (`:45-161`), the snapshot field
  (`savedRegisters [64][2]uint64`), the locals pool, and `localsSaveAreaPtr`
  (`:231-233`). In `handleThrow` (`:819-`), source per-frame `moduleInstance` from the frame's
  moduleCtx fixed slot (`moduleInstanceFromOpaquePtr`) and transfer to `EhClause.LandingPad`
  (with SP/FP from the walk) instead of `resumePC`.
- `engine_cache.go`: the side table already serializes; drop the now-unused
  `TryTableInfo.NumLocals/ReuseLocals` and bump the cache version (the existing
  "stale ⇒ recompile" path handles old entries).

## Phase breakdown (Sonnet MVP → Opus finalize)

- **P3.0 — backend fixed-slot primitive (Sonnet, low risk).** Reserve the two ctx slots, store
  in prologue, expose `EhCtxSlotOffsets()`, keep the *current* runtime otherwise. Assert (in a
  test) that a landing pad reloading execCtx from the slot equals the enter-time execCtx. No
  behavior change yet. Backstop: full EH corpus (`spectest/exception-handling`,
  `exceptions_test.go`, `eh_hammer_test.go`).
- **P3.1 — drop the snapshot, still resume at enter-continuation (Sonnet→Opus, med).** Give
  moduleCtx + cat-4 floor fixed-slot homes and reload at the enter-continuation/handler; stop
  restoring `savedRegisters` on the throw transfer. Enter still Go-exits (small win), but this
  de-risks the memory-homing before touching control flow. Prove against the C++ module that
  previously crashed (`experimental/exceptions_test.go::TestCppExceptions`, `eh_pdfium`).
- **P3.2 — zero-cost enter/leave via direct landing pad (Opus, the crux).** Emit self-
  sufficient landing pads (empty register live-in set), resume at `LandingPad`, recover
  `moduleInstance` per-frame, delete `ExitCodeTryTableEnter/Leave` + `activeCatchScopes` +
  snapshot + heap save area. Handle the tamed abnormal edge (keep the landing pad allocated
  with no register live-ins). Move cat-3 locals to frame slots. This is where enter/leave hit
  zero instructions and the ~90× shows up.
- **P3.3 — cleanup (Sonnet).** Remove `CompileThrowTransferRegisterRestore`, `restoreFn`, the
  `savedRegisters` field, `localsSaveAreaPtr`, `TryTableInfo.NumLocals/ReuseLocals`; bump the
  cache format; add the fast-path benchmark delta and new tests (throw across a stack grow;
  deep nesting; cross-module tag recovery on both ISAs).

## Risk ranking

1. **Tamed abnormal edge / self-sufficient landing pad (Q3, P3.2).** Regalloc must keep a
   block that has no normal CFG predecessor allocated and expect zero register live-ins. If any
   live-in is left register-sourced, it is silent corruption on the caught path. Mitigation:
   assert the landing pad's live-in register set is empty after lowering; the interpreter
   oracle + `eh_locals_*` corpus pins values; P3.1 lands the memory-homing first so P3.2 only
   changes control flow.
2. **Exact cat-3/cat-4 set (frontend).** Same silent-corruption risk as the original design's
   risk #1; usually trivial (empty floor, few catch-live locals). Oracle: interpreter
   `targetStackDepth`.
3. **Two-ISA prologue/slot ABI + per-frame module recovery (Q2/Q4/Q5).** Mechanical but
   touches both `.s`/prologue paths and the throw walk; cross-module/imported-call frames must
   each carry the moduleCtx slot. Lower risk (fixed-size frames, existing walk), broad surface.
   Test: throw after a forced stack grow, and wasm→wasm cross-module propagation, on amd64 and
   arm64.

## Honest verdict & win quantification

**Zero-cost enter is achievable at acceptable risk — GO, staged, Opus-owned.** The enabling
primitive (a fixed SP-relative frame slot for execCtx/moduleCtx) is small and reuses existing
machinery; it cleanly breaks the P2b circularity; every other primitive already ships. The one
subtle item (self-sufficient landing pad + tamed abnormal edge) is well-contained and is
de-risked by landing the memory-homing (P3.1) before the control-flow change (P3.2).

**The cheap partial is not worth shipping alone.** Measured on this box (amd64,
`BenchmarkEHFastPath` vs a reverted no-`try_table` floor, 1 s × 2):

| variant | ns/op (n=1000) | ns/iter |
|---|---|---|
| `try_table` every iter, never throws (baseline) | ~85 900 | ~86 |
| identical loop, `try_table` removed (floor) | ~940 | ~0.94 |

EH machinery is **~85 ns/iter ≈ 99%** of the fast path; the ~1 KB snapshot copy is a small
slice of that (the two Go mode-switch round trips dominate), so "drop only the snapshot" while
keeping the Go exits would capture only a low-single-digit-percent improvement — not worth the
memory-homing work on its own. The value is entirely in **deleting the two Go exits**, which
requires the full model. Delivered, P3 takes this microbenchmark from ~86 ns/iter toward the
~1 ns floor (plus a couple of frame stores for cat-3/4 homing) — **an order-of-magnitude
(~90×) speedup**, with whole-program wins scaling by enter density.

**What would change the calculus toward deferral:** if profiling of the actual target
workloads (pdfium/Emscripten) shows `try_table` enter is *not* on a hot path, the ~90× is
microbenchmark-only and P3 should wait behind higher-impact items. It should also wait if the
regalloc landing-pad work (risk #1) turns out to need deeper allocator surgery than the "keep
allocated, empty register live-ins" contract assumed here — that contract is the go/no-go gate
for P3.2 and should be validated with a spike before committing the phase.
