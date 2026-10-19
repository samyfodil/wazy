package native

import (
	"context"
	"encoding/binary"
	"fmt"
	"runtime"
	"sync/atomic"
	"unsafe"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/engine/native/backend"
	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
	"github.com/samyfodil/wazy/internal/expctxkeys"
	"github.com/samyfodil/wazy/internal/internalapi"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/wasm"
	"github.com/samyfodil/wazy/internal/wasmdebug"
	"github.com/samyfodil/wazy/internal/wasmruntime"
)

// savedRegistersLen is the number of 16-byte slots in executionContext.savedRegisters (and in the
// two snapshots of it below). Derived from the offsets the backends store through, so the Go struct
// and the generated code can never disagree about where the field ends -- see
// nativeapi.ExecutionContextOffsetSavedRegistersEnd for why it is sized this tightly.
const savedRegistersLen = (nativeapi.ExecutionContextOffsetSavedRegistersEnd -
	nativeapi.ExecutionContextOffsetSavedRegistersBegin) / 16

type (
	// callEngine implements api.Function.
	callEngine struct {
		internalapi.WazyOnly
		// slot records whether this callEngine is inside a top-level call, on
		// its own cache line rather than in a counter every caller shares, so
		// concurrent calls to one module do not serialize on it. Registered
		// with the module's memories in NewFunction; see wasm.CallSlot.
		slot  wasm.CallSlot
		stack []byte
		// stackTop is the pointer to the *aligned* top of the stack. This must be updated
		// whenever the stack is changed. This is passed to the assembly function
		// at the very beginning of api.Function Call/CallWithStack.
		stackTop uintptr
		// executable is the pointer to the executable code for this function.
		executable         *byte
		preambleExecutable *byte
		// parent is the *moduleEngine from which this callEngine is created.
		parent *moduleEngine
		// eng is the *engine that owns the shared stack pool (== parent.parent.
		// parent). Cached here by callWithStack's non-guard setup so growStack /
		// the grow loop can acquire and release stack buffers without re-walking
		// the parent chain (and so unit tests can wire a bare engine).
		eng *engine
		// indexInModule is the index of the function in the module.
		indexInModule wasm.Index
		// stackRetained means this callEngine has already returned one stack to the pool, so the
		// next call keeps its buffer in c.stack instead of doing a pool Get+Put per call. It lives
		// in the padding after indexInModule, so it costs this struct nothing.
		//
		// The first call always releases: mod.ExportedFunction(name).Call(ctx) builds a fresh
		// callEngine every time (see stack_pool.go), and a callEngine that is called once and
		// dropped must give its buffer back or the pool drains and every lookup allocates ~10KB
		// again. A callEngine called twice is a cached handle in a loop, where the pool round trip
		// is pure overhead. The tradeoff is a handle called exactly twice and then dropped, whose
		// buffer goes to the GC rather than back to the pool.
		stackRetained bool
		// sizeOfParamResultSlice is the size of the parameter/result slice.
		sizeOfParamResultSlice int
		requiredParams         int
		// execCtx holds various information to be read/written by assembly functions.
		execCtx executionContext
		// execCtxPtr holds the pointer to the executionContext which doesn't change after callEngine is created.
		execCtxPtr        uintptr
		numberOfResults   int
		stackIteratorImpl stackIterator
		// gcExec is this call's registration with the store's collector, nil unless the collector is on (see
		// wasm.CollectorEnabled). The first call allocates it and later ones re-register the same one, so a
		// call does not allocate.
		//
		// Nothing else about the collector lives here: this struct is allocated per call and sits just under
		// a Go size class boundary, so every field added to it costs a whole class, and a pointer field also
		// lengthens the prefix the allocator and the GC have to walk on every fresh callEngine.
		gcExec *wasm.GCExecution
		// pendingException holds the most recently caught exception, so handler
		// code can read its params after re-entry.
		pendingException *wasm.Exception
	}

	// executionContext is the struct to be read/written by assembly functions.
	executionContext struct {
		// exitCode holds the nativeapi.ExitCode describing the state of the function execution.
		exitCode nativeapi.ExitCode
		// gcPause is the flag loop headers poll: non-zero means a collection is waiting for this call to
		// park. It lives in the padding after exitCode, so it costs nothing. See ExecutionContextOffsetGCPause.
		gcPause uint32
		// callerModuleContextPtr holds the moduleContextOpaque for Go function calls.
		callerModuleContextPtr *byte
		// originalFramePointer holds the original frame pointer of the caller of the assembly function.
		originalFramePointer uintptr
		// originalStackPointer holds the original stack pointer of the caller of the assembly function.
		originalStackPointer uintptr
		// goReturnAddress holds the return address to go back to the caller of the assembly function.
		goReturnAddress uintptr
		// stackBottomPtr holds the check LIMIT used by the prologue/go-call
		// stack-bounds checks: the actual bottom of the c.stack buffer,
		// biased UP by backend.StackBoundsCheckMarginBytes (H7's MARGIN).
		// Biasing it here (rather than adding MARGIN into every function's
		// requiredStackSize() immediate) reserves the same slack while
		// keeping that immediate small on the hot path. See the invariant
		// proof on backend.StackBoundsCheckMarginBytes (backend/go_call.go).
		// stackBottomPtr is only ever read as this check limit -- never
		// used for pointer arithmetic back to the real buffer start -- so
		// biasing it is safe; the grow/clone/restore paths always re-derive
		// it fresh from the (possibly new) c.stack slice below.
		stackBottomPtr *byte
		// goCallReturnAddress holds the return address to go back to the caller of the Go function.
		goCallReturnAddress *byte
		// stackPointerBeforeGoCall holds the stack pointer before calling a Go function.
		stackPointerBeforeGoCall *uint64
		// stackGrowRequiredSize holds the required size of stack grow.
		stackGrowRequiredSize uintptr
		// memoryGrowTrampolineAddress holds the address of memory grow trampoline function.
		memoryGrowTrampolineAddress *byte
		// stackGrowCallTrampolineAddress holds the address of stack grow trampoline function.
		stackGrowCallTrampolineAddress *byte
		// checkModuleExitCodeTrampolineAddress holds the address of check-module-exit-code function.
		checkModuleExitCodeTrampolineAddress *byte
		// savedRegisters is the opaque spaces for save/restore registers.
		// We want to align 16 bytes for each register, so each slot is a [2]uint64.
		savedRegisters [savedRegistersLen][2]uint64
		// goFunctionCallCalleeModuleContextOpaque is the pointer to the target Go function's moduleContextOpaque.
		goFunctionCallCalleeModuleContextOpaque uintptr
		// tableGrowTrampolineAddress holds the address of table grow trampoline function.
		tableGrowTrampolineAddress *byte
		// refFuncTrampolineAddress holds the address of ref-func trampoline function.
		refFuncTrampolineAddress *byte
		// memmoveAddress holds the address of memmove function implemented by Go runtime. See memmove.go.
		memmoveAddress uintptr
		// framePointerBeforeGoCall holds the frame pointer before calling a Go function. Note: only used in amd64.
		framePointerBeforeGoCall uintptr
		// memoryWait32TrampolineAddress holds the address of memory_wait32 trampoline function.
		memoryWait32TrampolineAddress *byte
		// memoryWait32TrampolineAddress holds the address of memory_wait64 trampoline function.
		memoryWait64TrampolineAddress *byte
		// memoryNotifyTrampolineAddress holds the address of the memory_notify trampoline function.
		memoryNotifyTrampolineAddress *byte
		// throwAllocTrampolineAddress holds the address of the throw-alloc trampoline:
		// phase 1 of throw, which allocates the Exception heap object.
		throwAllocTrampolineAddress *byte
		// throwTrampolineAddress holds the address of the throw/throw_ref trampoline function.
		throwTrampolineAddress *byte
		// exceptionRef holds the exnref of the caught exception, which
		// catch_ref/catch_all_ref push. It is a handle into the store's
		// exception table rather than a pointer to the Exception, because
		// guest code may hold an exnref past this call and the collector
		// cannot see a Go pointer parked in wasm state. See
		// wasm.ExceptionTable.
		exceptionRef uintptr
		// exceptionParamsPtr points into the caught Exception's Params slice
		// backing array. On the throw side, throwAlloc sets it so compiled
		// code can store params at [ptr + i*8]. On the catch side, compiled
		// handler blocks load params from the same pointer.
		exceptionParamsPtr uintptr
		// caughtExceptionClauseIdx is non-zero while a throw resumes the
		// catching frame right after the call it was in. Compiled code checks it
		// after every call that can throw inside a try_table and branches to the
		// landing pad, which clears it. See (*callEngine).handleThrow.
		caughtExceptionClauseIdx int64
		// thrownTag is the *wasm.TagInstance of the exception being caught,
		// compared against the catch clauses by the landing pad.
		thrownTag uintptr
		// gcCheckTrampolineAddress holds the address of the GC runtime type check trampoline.
		gcCheckTrampolineAddress *byte
		// memclrAddress holds the address of memclrNoHeapPointers implemented by the Go
		// runtime. See memclr.go.
		memclrAddress uintptr
	}
)

// requiredInitialStackSize is how big a buffer a call needs when reserve bytes of it go to the GC root region.
func (c *callEngine) requiredInitialStackSize(reserve int) int {
	// stackPoolBaseSize (stack_pool.go) is this function's own default, kept
	// as a single source of truth since it is also pool size class 0's
	// canonical size -- see that file's doc comment.
	stackSize := stackPoolBaseSize
	paramResultInBytes := c.sizeOfParamResultSlice * 8 * 2 // * 8 because uint64 is 8 bytes, and *2 because we need both separated param/result slots.
	required := paramResultInBytes + 32 + 16               // 32 is enough to accommodate the call frame info, and 16 exists just in case when []byte is not aligned to 16 bytes.
	if reserve > 0 {
		// This much of the buffer is below the stack limit, so the guest never gets it. At least as much again
		// is left for the guest, so a large region does not leave it a sliver.
		required = max(required+reserve, 2*(reserve+backend.StackBoundsCheckMarginBytes))
	}
	if required > stackSize {
		stackSize = required
	}
	return stackSize
}

// init sets up the call-invariant fields of a freshly-constructed
// callEngine (see moduleEngine.NewFunction). Notably, in the default
// (StackGuardCheckEnabled == false) build, it does NOT allocate the wasm
// stack anymore: that used to be an unconditional
// `c.stack = make([]byte, requiredInitialStackSize())` here, i.e. a fresh
// ~10KB allocation on every single callEngine, hence on every
// mod.ExportedFunction(name).Call(ctx) (NewFunction builds a fresh
// callEngine per call -- ExportedFunction is never cached). That
// allocation is now done by (*callEngine).callWithStack, pulling from
// engine.stackPools (stack_pool.go) instead, and released back when that
// call returns -- except once this callEngine has proven to be a reused
// handle, after which it holds onto the buffer instead (see stackRetained).
//
// It can't just be done once here instead of in callWithStack: a
// callEngine created and called once, which is what
// mod.ExportedFunction(name).Call(ctx) produces every time, must hand its
// buffer back or the pool has nothing to hand out.
func (c *callEngine) init() {
	if nativeapi.StackGuardCheckEnabled {
		// Debug-only path: never pooled -- see acquireStack's doc comment
		// for why (in short: CheckStackGuardPage requires the guard page
		// to still be all-zero, which a reused buffer cannot guarantee) --
		// so it keeps the simple eager-allocate-once-here behavior.
		reserve := c.gcRootsReserve()
		stackSize := c.requiredInitialStackSize(reserve) + nativeapi.StackGuardCheckGuardPageSize
		c.stack = platform.MakeStack(stackSize)
		c.stackTop = alignedStackTop(c.stack)
		c.execCtx.stackBottomPtr = c.stackCheckLimitPtr(nativeapi.StackGuardCheckGuardPageSize, reserve)
	}
	c.execCtxPtr = uintptr(unsafe.Pointer(&c.execCtx))
}

// alignedStackTop returns 16-bytes aligned stack top of given stack.
// 16 bytes should be good for all platform (arm64/amd64).
func alignedStackTop(s []byte) uintptr {
	stackAddr := uintptr(unsafe.Pointer(&s[len(s)-1]))
	return stackAddr - (stackAddr & (16 - 1))
}

// stackCheckLimitPtr returns the value to store in
// executionContext.stackBottomPtr: the stack-bounds-check LIMIT, i.e. the
// logical bottom of the usable stack (stack[base]) biased UP by H7's
// backend.StackBoundsCheckMarginBytes (MARGIN). Every assignment to
// stackBottomPtr goes through here so the MARGIN bias can never be forgotten
// by a future edit -- omitting it would under-reserve the guard and risk a
// stack overflow. base is 0 for a plain pooled/grown buffer, or
// nativeapi.StackGuardCheckGuardPageSize for the debug guard-page build. The
// indexing is always in range: the real buffer is >= stackPoolBaseSize
// (10240) and only ever grows, so base+MARGIN < len(stack) always holds. See
// the invariant proof on backend.StackBoundsCheckMarginBytes.
//
// reserve is the size of the GC root region below the limit (see gcRootsReserve), which the caller makes sure
// fits: at the start of a call it is the engine's and the buffer was sized for it, and in the middle of one it
// is rootsReserveInUse, the one the call started with.
func (c *callEngine) stackCheckLimitPtr(base, reserve int) *byte {
	if reserve > 0 {
		// A stack fresh from the pool holds another call's words; a zero count keeps them from reading as this
		// call's roots. The high half is how many words the region holds.
		binary.LittleEndian.PutUint64(c.stack[base+reserve-8:], uint64((reserve-8)/8)<<32)
	}
	return &c.stack[base+reserve+backend.StackBoundsCheckMarginBytes]
}

// prepareStack gets this call a wasm stack -- the one kept from the last call if it is still big enough, or
// one from eng's pool, returned boxed for releasing -- and places its limit and GC root region. It is out of
// line so the values it works with take no room in callWithStack's frame, which every exit runs on.
//
//go:noinline
func (c *callEngine) prepareStack(eng *engine) (stackBoxed *[]byte) {
	reserve := c.gcRootsReserve()
	if c.stack != nil && reserve > 0 && len(c.stack) < c.requiredInitialStackSize(reserve) {
		// Kept from an earlier call, before the engine's root region outgrew it.
		eng.releaseStack(c.stack, nil)
		c.stack = nil
	}
	if c.stack == nil {
		c.stack, stackBoxed = eng.acquireStack(c.requiredInitialStackSize(reserve))
		c.stackTop = alignedStackTop(c.stack)
	}
	// Recomputed even on a retained buffer: it is what clears the previous call's spilled-root count, which
	// must not read as this call's (see stackCheckLimitPtr).
	c.execCtx.stackBottomPtr = c.stackCheckLimitPtr(0, reserve)
	return
}

// rootsReserveInUse is the size of the root region under the current stack limit, read back from its header.
// A call keeps the region it started with until it returns: one placed again for a larger engine reserve could
// land on frames the call already has.
func (c *callEngine) rootsReserveInUse() int {
	if c.parent == nil || c.parent.gcRootsReserve == nil || c.execCtx.stackBottomPtr == nil {
		return 0
	}
	hdr := int(uintptr(unsafe.Pointer(c.execCtx.stackBottomPtr))-uintptr(unsafe.Pointer(&c.stack[0]))) -
		backend.StackBoundsCheckMarginBytes - 8
	return int(binary.LittleEndian.Uint64(c.stack[hdr:])>>32)*8 + 8
}

// registerGC makes this call visible to the collector, which may then stop it and scan its stack. Only ever
// called once this callEngine's stack is one it owns. It runs on every call, so it stays small enough to
// inline into callWithStack, where a runtime without the collector pays only the flag check; the rest is in
// registerGCSlow.
func (c *callEngine) registerGC() {
	if c.parent.gcEnabled {
		c.registerGCSlow()
	}
}

func (c *callEngine) registerGCSlow() {
	if c.gcExec == nil {
		c.gcExec = new(wasm.GCExecution)
	}
	c.parent.module.RegisterGCExecutionInto(c.gcExec, c, &c.execCtx.gcPause)
}

// unregisterGC undoes registerGC. gcExec is kept for the next call, and Unregister is a no-op on one that is
// not registered, so the check below is only to skip the call where there is no collector.
func (c *callEngine) unregisterGC() {
	if c.gcExec != nil {
		c.gcExec.Unregister()
	}
}

// gcRootsReserve is how many bytes at the bottom of the wasm stack are set aside for the values a safepoint
// spills before it parks. The stack limit is biased up by this much on top of the usual margin, so the region
// sits below everything generated code and the go-call trampoline can reach, and nothing overwrites a spill
// while the collector is looking at it.
//
// Putting them here rather than in a buffer of their own costs nothing on the per-call path: no second pool to
// draw from, and no pointer to keep in the execution context, because compiled code already holds the stack
// limit at a fixed offset there and the region is a constant distance below it.
//
// The layout, from the bottom of the buffer up, is: the spilled words, then a header word, then the go-call
// margin, then the stack the guest actually gets. The header's high half is how many words the region holds and
// its low half how many the last safepoint wrote (see nativeapi.GCRootsMissing).
//
// The size is the engine's, not the module's: a call runs whatever functions it reaches, through imports and
// tables, and every one of them spills into this same region. It covers every module the engine had
// instantiated when the call placed its limit; see engine.noteGCRoots.
func (c *callEngine) gcRootsReserve() int {
	if c.parent == nil || c.parent.gcRootsReserve == nil {
		return 0 // no collector, or a callEngine built by hand in a test with no module behind it.
	}
	return int(c.parent.gcRootsReserve.Load())
}

// Definition implements api.Function.
func (c *callEngine) Definition() api.FunctionDefinition {
	return c.parent.module.Source.FunctionDefinition(c.indexInModule)
}

// Call implements api.Function.
func (c *callEngine) Call(ctx context.Context, params ...uint64) ([]uint64, error) {
	if c.requiredParams != len(params) {
		return nil, fmt.Errorf("expected %d params, but passed %d", c.requiredParams, len(params))
	}
	paramResultSlice := make([]uint64, c.sizeOfParamResultSlice)
	copy(paramResultSlice, params)
	if err := c.callWithStack(ctx, paramResultSlice); err != nil {
		return nil, err
	}
	return paramResultSlice[:c.numberOfResults], nil
}

func (c *callEngine) addFrame(builder wasmdebug.ErrorBuilder, addr uintptr) (def api.FunctionDefinition, listener api.FunctionListener) {
	eng := c.parent.parent.parent
	cm := eng.compiledModuleOfAddr(addr)
	if cm == nil {
		// This case, the module might have been closed and deleted from the engine.
		// We fall back to searching the imported modules that can be referenced from this callEngine.

		// First, we check itself.
		if checkAddrInBytes(addr, c.parent.parent.executable) {
			cm = c.parent.parent
		} else {
			// Otherwise, search all imported modules. TODO: maybe recursive, but not sure it's useful in practice.
			p := c.parent
			for i := range p.importedFunctions {
				candidate := p.importedFunctions[i].me.parent
				if checkAddrInBytes(addr, candidate.executable) {
					cm = candidate
					break
				}
			}
		}
	}

	if cm != nil {
		index := cm.functionIndexOf(addr)
		def = cm.module.FunctionDefinition(cm.module.ImportFunctionCount + index)
		var sources []string
		if dw := cm.module.DWARFLines; dw != nil {
			sourceOffset := cm.getSourceOffset(addr)
			sources = dw.Line(sourceOffset)
		}
		builder.AddFrame(def.DebugName(), def.ParamTypes(), def.ResultTypes(), sources)
		if len(cm.listeners) > 0 {
			listener = cm.listeners[index]
		}
	}
	return
}

// outOfFuel is the body of the ExitCodeCheckModuleExitCode arm of the dispatch
// loop below: the guest spent its last tick of termination fuel
// (nativeapi.TerminationFuel) and is back in Go for the two things it cannot do
// itself.
//
// The first is the check. Whether the module has been closed is state only Go can
// read atomically, and closing it is what the deadline watchdog does.
//
// The second is the yield, and it is the reason this exit exists at all. Compiled
// code is entered through asm trampolines marked unsafe for async preemption, so a
// guest that never calls out holds its P for as long as it runs: the watchdog
// goroutine that would close the module cannot be scheduled, and neither can a
// stop-the-world. Handing the P back here is what makes both possible -- it is
// what an entersyscall/exitsyscall bracket around every entry into compiled code
// used to buy, at 28.5ns (amd64) on every host-call return rather than once per
// 8192 fuel ticks.
//
// Then the check again, because the yield is exactly the window the watchdog
// needed: without the second look a close landing during it would wait for the
// next time the fuel ran out, doubling the worst-case latency for one atomic load.
//
// It is a function of its own rather than twenty lines inside the switch because
// the loop it belongs to is also the hot path for host calls, and that loop is
// easier to read when each arm is short.
func outOfFuel(m *wasm.ModuleInstance) {
	if err := m.FailIfClosed(); err != nil {
		panic(err)
	}
	runtime.Gosched()
	if err := m.FailIfClosed(); err != nil {
		panic(err)
	}
}

// CallWithStack implements api.Function.
func (c *callEngine) CallWithStack(ctx context.Context, paramResultStack []uint64) (err error) {
	if c.sizeOfParamResultSlice > len(paramResultStack) {
		return fmt.Errorf("need %d params, but stack size is %d", c.sizeOfParamResultSlice, len(paramResultStack))
	}
	return c.callWithStack(ctx, paramResultStack)
}

// CallWithStack implements api.Function.
func (c *callEngine) callWithStack(ctx context.Context, paramResultStack []uint64) (err error) {
	snapshotEnabled := expctxkeys.SnapshotterEnabled.Load() && ctx.Value(expctxkeys.EnableSnapshotterKey{}) != nil
	if snapshotEnabled {
		ctx = context.WithValue(ctx, expctxkeys.SnapshotterKey{}, c)
	}

	if nativeapi.StackGuardCheckEnabled {
		// This build's stack is the one init() allocated and never recycles, so it is already stable to scan.
		c.registerGC()
		defer func() {
			c.unregisterGC()
			nativeapi.CheckStackGuardPage(c.stack)
		}()
	} else {
		// Acquire this top-level call's wasm stack from the shared pool
		// (stack_pool.go) instead of allocating a fresh one, and release it
		// back when this call returns -- via defer, so it happens on every
		// exit path, including the ctx-already-done early return
		// immediately below and a panic/trap unwinding through the
		// recover() defer further down.
		//
		// This defer is registered before (and therefore, per Go's LIFO
		// defer order, runs *after*) the recover-handling defer below: that
		// defer's own work (building a trap backtrace via unwindStack,
		// which reads through c.stackTop/c.stack) must finish before this
		// buffer is handed back to the pool for some other goroutine to
		// reuse.
		eng := c.parent.parent.parent
		c.eng = eng // so growStack / the grow loop can reach the pool
		var stackBoxed *[]byte
		if c.stack != nil && c.parent.gcRootsReserve == nil {
			// The common case, a kept stack and no collector, stays inline.
			c.execCtx.stackBottomPtr = c.stackCheckLimitPtr(0, 0)
		} else {
			stackBoxed = c.prepareStack(eng)
		}
		acquiredLen := len(c.stack)
		// Registered only now: a collection reads c.stack, so this call must own one first.
		c.registerGC()
		defer func() {
			// Unregister first: a collection may call ScanGCRoots right up until it returns, and that
			// reads c.stack -- which the release below hands to whichever call takes it out of the pool
			// next. Doing it here rather than in a defer of its own also makes the ordering a property
			// of the code instead of one of Go's defer order, and leaves the number of defers in this
			// function where it was, which is what keeps them open-coded.
			c.unregisterGC()
			if c.stackRetained && len(c.stack) == acquiredLen {
				return // kept for this callEngine's next call; see stackRetained.
			}
			c.stackRetained = true
			// Release whichever buffer c.stack currently is -- not
			// necessarily the one just acquired above: stack growth
			// (growStack/cloneStack) or an experimental Snapshot restore
			// (doRestore) may have replaced it with a different buffer
			// mid-call. Exactly one buffer is released here, exactly once,
			// matching the exactly-one acquire above; any old,
			// pre-growth/pre-restore buffer is simply not returned to the
			// pool and left for the GC, same as before pooling existed.
			eng.releaseStack(c.stack, stackBoxed)
			c.stack = nil
		}()
	}

	p := c.parent
	ensureTermination := p.parent.ensureTermination
	m := p.module
	if ensureTermination {
		select {
		case <-ctx.Done():
			// If the provided context is already done, close the module and return the error.
			m.CloseWithCtxErr(ctx)
			return m.FailIfClosed()
		default:
		}
	}

	var paramResultPtr *uint64
	if len(paramResultStack) > 0 {
		paramResultPtr = &paramResultStack[0]
	}
	defer func() {
		r := recover()
		if s, ok := r.(*snapshot); ok {
			// A snapshot that wasn't handled was created by a different call engine possibly from a nested wasm invocation,
			// let it propagate up to be handled by the caller. Release first
			// what a context-done close of this module deferred: the unwind
			// skips the FailIfClosed below, and nothing later reaches it.
			_ = c.parent.module.FailIfClosed()
			panic(s)
		}
		if r != nil {
			type listenerForAbort struct {
				def api.FunctionDefinition
				lsn api.FunctionListener
			}

			var listeners []listenerForAbort
			builder := wasmdebug.NewErrorBuilder()
			if c.execCtx.stackPointerBeforeGoCall != nil {
				def, lsn := c.addFrame(builder, uintptr(unsafe.Pointer(c.execCtx.goCallReturnAddress)))
				if lsn != nil {
					listeners = append(listeners, listenerForAbort{def, lsn})
				}
				returnAddrs := unwindStack(
					uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall)),
					c.execCtx.framePointerBeforeGoCall,
					c.stackTop,
					nil,
				)
				if len(returnAddrs) > 1 {
					for _, retAddr := range returnAddrs[:len(returnAddrs)-1] { // the last return addr is the trampoline, so we skip it.
						def, lsn = c.addFrame(builder, retAddr)
						if lsn != nil {
							listeners = append(listeners, listenerForAbort{def, lsn})
						}
					}
				}
			}
			err = builder.FromRecovered(r)

			for _, lsn := range listeners {
				lsn.lsn.Abort(ctx, m, lsn.def, err)
			}
		}

		// Call FailIfClosed however the call ended -- trap, proc_exit, host
		// panic or stack overflow, not only a normal return: if the context
		// watcher closed the module while it ran, this is what releases the
		// resources whose closing it deferred, and nothing later would.
		closeErr := c.parent.module.FailIfClosed()
		if r == nil && err != wasmruntime.ErrRuntimeStackOverflow { // Stackoverflow case shouldn't be panic (to avoid extreme stack unwinding).
			err = closeErr
		}

		if err != nil {
			// Ensures that we can reuse this callEngine even after an error.
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			c.execCtx.caughtExceptionClauseIdx = 0
		}
	}()

	if ensureTermination {
		done := m.CloseModuleOnCanceledOrTimeout(ctx)
		defer done()
	}

	// Compiled code holds the memory base directly and never checks whether
	// the module was closed, so a concurrent close must not recycle the buffer
	// until this call is done with it. See MemoryInstance.callers.
	c.slot.Enter()
	defer m.ExitCall(&c.slot)

	// Refuse a call that arrives after the module closed. Enter above is a
	// sequentially consistent store and this is the paired load, against a
	// close that sets Closed and then scans the slots -- so either the close
	// sees this call in flight and defers releasing the memory, or this call
	// sees the close and does not run. Without it, a handle taken before the
	// close still executes, against a buffer already back in the pool.
	//
	// It goes out through outOfFuel -- the same panic a close landing MID-call
	// already takes, caught by the recover above -- rather than returning. A
	// return here is one more than this function's defers can carry: they are
	// open-coded only while defers*returns stays within Go's budget, and
	// breaking that measured 40% worse on four cores.
	if m.Closed.Load() != 0 {
		outOfFuel(m)
	}

	if c.stackTop&(16-1) != 0 {
		panic("BUG: stack must be aligned to 16 bytes")
	}
	entrypointAsm(c.preambleExecutable, c.executable, c.execCtxPtr, c.parent.opaquePtr, paramResultPtr, c.stackTop)
	for {
		switch ec := c.execCtx.exitCode; ec & nativeapi.ExitCodeMask {
		case nativeapi.ExitCodeOK:
			return nil
		case nativeapi.ExitCodeGrowStack:
			oldsp := uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall))
			oldTop := c.stackTop
			oldStack := c.stack
			var newsp, newfp uintptr
			pooled := false
			if nativeapi.StackGuardCheckEnabled {
				newsp, newfp, err = c.growStackWithGuarded()
			} else {
				newsp, newfp, err = c.growStack()
				pooled = true // non-guard path buffers come from / go back to the stack pool
			}
			if err != nil {
				return err
			}
			adjustClonedStack(oldsp, oldTop, newsp, newfp, c.stackTop)
			if pooled {
				// oldStack is dead here: adjustClonedStack only reads/writes the
				// NEW buffer (translating the copied frame pointers), using the
				// old addresses purely as arithmetic bounds -- it never
				// dereferences oldStack. growStack replaced c.stack/c.stackTop and
				// reset stackBottomPtr to the new buffer, and execution resumes on
				// newsp/newfp below. So recycle oldStack into the stack pool
				// instead of dropping it to GC: growStack DOUBLES on every grow, so
				// a deep-growth guest would otherwise allocate AND discard every
				// intermediate buffer of the doubling sequence (measured at 2.12 GB
				// across the spec suite's stack-overflow tests). releaseStack keeps
				// oldStack alive across the pool hand-off, subsuming the KeepAlive
				// the GC-only path below still needs. The final (grown) buffer is
				// released once more by callWithStack's defer -- each buffer in the
				// chain is released exactly once. Guard-check builds never pool
				// (init's debug path make()s the buffer), so they keep the
				// GC path.
				c.eng.releaseStack(oldStack, nil)
			} else {
				// Old stack must be alive until the new stack is adjusted.
				runtime.KeepAlive(oldStack)
			}
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			afterGoFunctionCallEntrypointAsm(c.execCtx.goCallReturnAddress, c.execCtxPtr, newsp, newfp)
		case nativeapi.ExitCodeGrowMemory:
			mod := c.callerModuleInstance()
			s := goCallStackView(c.execCtx.stackPointerBeforeGoCall)
			memIndex := uint32(s[0])
			mem := mod.Memories[memIndex]
			// The trampoline passes the page delta as a full 64 bits whatever the
			// memory's index type; a 32-bit memory's fits in the low half.
			pages := s[1]
			if res, ok := mem.Grow64(pages); !ok {
				if mem.IsMemory64 {
					s[0] = ^uint64(0) // = -1 in signed 64-bit integer.
				} else {
					s[0] = uint64(0xffffffff) // = -1 in signed 32-bit integer.
				}
			} else {
				s[0] = res
			}
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			afterGoFunctionCallEntrypointAsm(c.execCtx.goCallReturnAddress, c.execCtxPtr, uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall)), c.execCtx.framePointerBeforeGoCall)
		case nativeapi.ExitCodeTableGrow:
			mod := c.callerModuleInstance()
			s := goCallStackView(c.execCtx.stackPointerBeforeGoCall)
			// As with memory.grow, the trampoline passes the entry count as a
			// full 64 bits whatever the table's index type.
			tableIndex, num, ref := uint32(s[0]), s[1], uintptr(s[2])
			table := mod.Tables[tableIndex]
			if table.IsTable64 {
				s[0] = table.Grow64(num, ref)
			} else {
				s[0] = uint64(table.Grow(uint32(num), ref))
			}
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			afterGoFunctionCallEntrypointAsm(c.execCtx.goCallReturnAddress, c.execCtxPtr,
				uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall)), c.execCtx.framePointerBeforeGoCall)
		case nativeapi.ExitCodeCallGoFunction:
			index := nativeapi.GoFunctionIndexFromExitCode(ec)
			f := hostModuleGoFuncFromOpaque[api.GoFunction](index, c.execCtx.goFunctionCallCalleeModuleContextOpaque)
			stack := goCallStackView(c.execCtx.stackPointerBeforeGoCall)
			// A host function can block for as long as it likes, and this call's wasm stack does not move
			// while it does, so this is a parking point too: otherwise a collection would wait on an
			// execution that is never going to reach a loop header.
			c.gcExec.EnterGo()
			if snapshotEnabled {
				callGoFunctionWithSnapshotRecover(c, ctx, f, stack)
			} else {
				f.Call(ctx, stack)
			}
			c.gcExec.LeaveGo()
			// Back to the native code.
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			afterGoFunctionCallEntrypointAsm(c.execCtx.goCallReturnAddress, c.execCtxPtr,
				uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall)), c.execCtx.framePointerBeforeGoCall)
		case nativeapi.ExitCodeCallGoFunctionWithListener:
			index := nativeapi.GoFunctionIndexFromExitCode(ec)
			f := hostModuleGoFuncFromOpaque[api.GoFunction](index, c.execCtx.goFunctionCallCalleeModuleContextOpaque)
			listeners := hostModuleListenersSliceFromOpaque(c.execCtx.goFunctionCallCalleeModuleContextOpaque)
			s := goCallStackView(c.execCtx.stackPointerBeforeGoCall)
			// Call Listener.Before.
			callerModule := c.callerModuleInstance()
			listener := listeners[index]
			hostModule := hostModuleFromOpaque(c.execCtx.goFunctionCallCalleeModuleContextOpaque)
			def := hostModule.FunctionDefinition(wasm.Index(index))
			listener.Before(ctx, callerModule, def, s, c.stackIterator(true))
			// Call into the Go function. As above, this is a parking point.
			c.gcExec.EnterGo()
			if snapshotEnabled {
				callGoFunctionWithSnapshotRecover(c, ctx, f, s)
			} else {
				f.Call(ctx, s)
			}
			c.gcExec.LeaveGo()
			// Call Listener.After.
			listener.After(ctx, callerModule, def, s)
			// Back to the native code.
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			afterGoFunctionCallEntrypointAsm(c.execCtx.goCallReturnAddress, c.execCtxPtr,
				uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall)), c.execCtx.framePointerBeforeGoCall)
		case nativeapi.ExitCodeCallGoModuleFunction:
			index := nativeapi.GoFunctionIndexFromExitCode(ec)
			f := hostModuleGoFuncFromOpaque[api.GoModuleFunction](index, c.execCtx.goFunctionCallCalleeModuleContextOpaque)
			mod := c.callerModuleInstance()
			stack := goCallStackView(c.execCtx.stackPointerBeforeGoCall)
			// A host function can block for as long as it likes; see ExitCodeCallGoFunction above.
			c.gcExec.EnterGo()
			if snapshotEnabled {
				callGoModuleFunctionWithSnapshotRecover(c, ctx, f, mod, stack)
			} else {
				f.Call(ctx, mod, stack)
			}
			c.gcExec.LeaveGo()
			// Back to the native code.
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			afterGoFunctionCallEntrypointAsm(c.execCtx.goCallReturnAddress, c.execCtxPtr,
				uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall)), c.execCtx.framePointerBeforeGoCall)
		case nativeapi.ExitCodeCallGoModuleFunctionWithListener:
			index := nativeapi.GoFunctionIndexFromExitCode(ec)
			f := hostModuleGoFuncFromOpaque[api.GoModuleFunction](index, c.execCtx.goFunctionCallCalleeModuleContextOpaque)
			listeners := hostModuleListenersSliceFromOpaque(c.execCtx.goFunctionCallCalleeModuleContextOpaque)
			s := goCallStackView(c.execCtx.stackPointerBeforeGoCall)
			// Call Listener.Before.
			callerModule := c.callerModuleInstance()
			listener := listeners[index]
			hostModule := hostModuleFromOpaque(c.execCtx.goFunctionCallCalleeModuleContextOpaque)
			def := hostModule.FunctionDefinition(wasm.Index(index))
			listener.Before(ctx, callerModule, def, s, c.stackIterator(true))
			// Call into the Go function. As above, this is a parking point.
			c.gcExec.EnterGo()
			if snapshotEnabled {
				callGoModuleFunctionWithSnapshotRecover(c, ctx, f, callerModule, s)
			} else {
				f.Call(ctx, callerModule, s)
			}
			c.gcExec.LeaveGo()
			// Call Listener.After.
			listener.After(ctx, callerModule, def, s)
			// Back to the native code.
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			afterGoFunctionCallEntrypointAsm(c.execCtx.goCallReturnAddress, c.execCtxPtr,
				uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall)), c.execCtx.framePointerBeforeGoCall)
		case nativeapi.ExitCodeCallListenerBefore:
			stack := goCallStackView(c.execCtx.stackPointerBeforeGoCall)
			index := wasm.Index(stack[0])
			mod := c.callerModuleInstance()
			listener := mod.Engine.(*moduleEngine).listeners[index]
			def := mod.Source.FunctionDefinition(index + mod.Source.ImportFunctionCount)
			listener.Before(ctx, mod, def, stack[1:], c.stackIterator(false))
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			afterGoFunctionCallEntrypointAsm(c.execCtx.goCallReturnAddress, c.execCtxPtr,
				uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall)), c.execCtx.framePointerBeforeGoCall)
		case nativeapi.ExitCodeCallListenerAfter:
			stack := goCallStackView(c.execCtx.stackPointerBeforeGoCall)
			index := wasm.Index(stack[0])
			mod := c.callerModuleInstance()
			listener := mod.Engine.(*moduleEngine).listeners[index]
			def := mod.Source.FunctionDefinition(index + mod.Source.ImportFunctionCount)
			listener.After(ctx, mod, def, stack[1:])
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			afterGoFunctionCallEntrypointAsm(c.execCtx.goCallReturnAddress, c.execCtxPtr,
				uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall)), c.execCtx.framePointerBeforeGoCall)
		case nativeapi.ExitCodeCheckModuleExitCode:
			outOfFuel(m)
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			afterGoFunctionCallEntrypointAsm(c.execCtx.goCallReturnAddress, c.execCtxPtr,
				uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall)), c.execCtx.framePointerBeforeGoCall)
		case nativeapi.ExitCodeRefFunc:
			mod := c.callerModuleInstance()
			s := goCallStackView(c.execCtx.stackPointerBeforeGoCall)
			funcIndex := wasm.Index(s[0])
			ref := mod.Engine.FunctionInstanceReference(funcIndex)
			s[0] = uint64(ref)
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			afterGoFunctionCallEntrypointAsm(c.execCtx.goCallReturnAddress, c.execCtxPtr,
				uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall)), c.execCtx.framePointerBeforeGoCall)
		case nativeapi.ExitCodeGCCheck:
			mod := c.callerModuleInstance()
			s := goCallStackView(c.execCtx.stackPointerBeforeGoCall)
			if s[0] == wasm.GCSafepoint {
				// Not a type check: the collector has asked this call to park. It shares the trampoline
				// rather than having one of its own, to keep a field out of the execution context.
				c.gcExec.Safepoint()
				s[0] = 0
			} else {
				// The native engine never uses the variadic forms directly: struct.new and
				// array.new_fixed are lowered as an allocation followed by one set per operand, so
				// there is no scratch area.
				s[0] = wasm.RunGC(mod, s[0], s[1], s[2], s[3], s[4], s[5], nil)
			}
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			afterGoFunctionCallEntrypointAsm(c.execCtx.goCallReturnAddress, c.execCtxPtr,
				uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall)), c.execCtx.framePointerBeforeGoCall)
		case nativeapi.ExitCodeMemoryWait32:
			mod := c.callerModuleInstance()
			s := goCallStackView(c.execCtx.stackPointerBeforeGoCall)
			memIndex, timeout, exp, addr := uint32(s[0]), int64(s[1]), uint32(s[2]), uintptr(s[3])
			mem := mod.Memories[memIndex]
			if !mem.Shared {
				panic(wasmruntime.ErrRuntimeExpectedSharedMemory)
			}

			base := uintptr(unsafe.Pointer(&mem.Buffer[0]))

			offset := uint64(addr - base)
			res := mem.Wait32(offset, exp, timeout, func(mem *wasm.MemoryInstance, offset uint64) uint32 {
				addr := unsafe.Add(unsafe.Pointer(&mem.Buffer[0]), offset)
				return atomic.LoadUint32((*uint32)(addr))
			})
			s[0] = res
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			afterGoFunctionCallEntrypointAsm(c.execCtx.goCallReturnAddress, c.execCtxPtr,
				uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall)), c.execCtx.framePointerBeforeGoCall)
		case nativeapi.ExitCodeMemoryWait64:
			mod := c.callerModuleInstance()
			s := goCallStackView(c.execCtx.stackPointerBeforeGoCall)
			memIndex, timeout, exp, addr := uint32(s[0]), int64(s[1]), uint64(s[2]), uintptr(s[3])
			mem := mod.Memories[memIndex]
			if !mem.Shared {
				panic(wasmruntime.ErrRuntimeExpectedSharedMemory)
			}

			base := uintptr(unsafe.Pointer(&mem.Buffer[0]))

			offset := uint64(addr - base)
			res := mem.Wait64(offset, exp, timeout, func(mem *wasm.MemoryInstance, offset uint64) uint64 {
				addr := unsafe.Add(unsafe.Pointer(&mem.Buffer[0]), offset)
				return atomic.LoadUint64((*uint64)(addr))
			})
			s[0] = uint64(res)
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			afterGoFunctionCallEntrypointAsm(c.execCtx.goCallReturnAddress, c.execCtxPtr,
				uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall)), c.execCtx.framePointerBeforeGoCall)
		case nativeapi.ExitCodeMemoryNotify:
			mod := c.callerModuleInstance()
			s := goCallStackView(c.execCtx.stackPointerBeforeGoCall)
			memIndex, count, addr := uint32(s[0]), uint32(s[1]), s[2]
			mem := mod.Memories[memIndex]
			offset := uint64(uintptr(addr) - uintptr(unsafe.Pointer(&mem.Buffer[0])))
			res := mem.Notify(offset, count)
			s[0] = uint64(res)
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			afterGoFunctionCallEntrypointAsm(c.execCtx.goCallReturnAddress, c.execCtxPtr,
				uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall)), c.execCtx.framePointerBeforeGoCall)
		case nativeapi.ExitCodeUnreachable:
			// frameReserve is never used for anything: touching it on this cold path holds this function's
			// frame at 0x4c8 bytes on amd64, the size it had before the exception
			// exits left this loop. Every exit runs on this frame, and on the Atom C3558 shrinking it by
			// those 128 bytes cost GCCall/walk (an exit per link) 3.5-5% in every code placement tried.
			var frameReserve [15]uint64
			runtime.KeepAlive(frameReserve[(ec>>8)%15])
			panic(wasmruntime.ErrRuntimeUnreachable)
		case nativeapi.ExitCodeMemoryOutOfBounds:
			panic(wasmruntime.ErrRuntimeOutOfBoundsMemoryAccess)
		case nativeapi.ExitCodeTableOutOfBounds:
			panic(wasmruntime.ErrRuntimeInvalidTableAccess)
		case nativeapi.ExitCodeIndirectCallNullPointer:
			panic(wasmruntime.ErrRuntimeInvalidTableAccess)
		case nativeapi.ExitCodeIndirectCallTypeMismatch:
			panic(wasmruntime.ErrRuntimeIndirectCallTypeMismatch)
		case nativeapi.ExitCodeIntegerOverflow:
			panic(wasmruntime.ErrRuntimeIntegerOverflow)
		case nativeapi.ExitCodeIntegerDivisionByZero:
			panic(wasmruntime.ErrRuntimeIntegerDivideByZero)
		case nativeapi.ExitCodeInvalidConversionToInteger:
			panic(wasmruntime.ErrRuntimeInvalidConversionToInteger)
		case nativeapi.ExitCodeUnalignedAtomic:
			panic(wasmruntime.ErrRuntimeUnalignedAtomic)
		case nativeapi.ExitCodeThrowAlloc:
			// Allocate the Exception heap object sized exactly to the tag's
			// param count. Sets exceptionParamsPtr so compiled code can
			// store params, and returns the exnref via the stack slot.
			s := goCallStackView(c.execCtx.stackPointerBeforeGoCall)
			tagIndex := int(s[0])
			mod := c.callerModuleInstance()
			tag := mod.Tags[tagIndex]
			nParams := len(tag.Type.Params)
			exn := &wasm.Exception{Tag: tag, Params: make([]uint64, nParams)}
			c.pendingException = exn // keeps exn reachable while compiled code writes params
			if nParams > 0 {
				c.execCtx.exceptionParamsPtr = uintptr(unsafe.Pointer(&exn.Params[0]))
			}
			// Return the exnref to compiled code via the stack slot. This is a handle into the store's table, not
			// a pointer: guest code can hold an exnref for as long as it likes -- in a local, or an exnref-typed
			// global that outlives this call -- and the collector cannot see a pointer parked there. See
			// wasm.ExceptionTable.
			s[0] = mod.ExceptionTable().Ref(exn)
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			afterGoFunctionCallEntrypointAsm(c.execCtx.goCallReturnAddress, c.execCtxPtr,
				uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall)), c.execCtx.framePointerBeforeGoCall)
		case nativeapi.ExitCodeThrow:
			// Throw trampoline: (execCtx, exnref) → ().
			// Reads the exnref from the stack, then walks the native stack
			// against the per-function exception side table and transfers
			// control directly to a matching landing pad -- see handleThrow.
			s := goCallStackView(c.execCtx.stackPointerBeforeGoCall)
			exn, ok := c.callerModuleInstance().ExceptionTable().Lookup(s[0])
			if !ok {
				// An exnref this store never handed out. Guest code cannot forge one -- no instruction converts
				// an integer to an exnref -- so this is a null or foreign handle, and resolving it would be a
				// wild pointer. See wasm.ExceptionTable.
				panic(wasmruntime.ErrRuntimeNullReference)
			}
			if !c.handleThrow(exn) {
				panic(wasmruntime.ErrRuntimeUncaughtException)
			}
			// handleThrow already transferred control on a match (the
			// destination PC/SP/FP are the matched landing pad's, not
			// goCallReturnAddress/stackPointerBeforeGoCall like every other
			// exit code's uniform "resume where we left off").
		case nativeapi.ExitCodeNullReference:
			panic(wasmruntime.ErrRuntimeNullReference)
		default:
			panic("BUG")
		}
	}
}

// handleThrow finds the nearest frame in the current native stack segment
// that is stopped at a call inside a try_table with catch clauses (its
// return address falls in one of compiledModule.ehTables' ranges), walking
// outward from the throw site, and resumes that frame right after the call.
// It sets execCtx.caughtExceptionClauseIdx so the check the compiler put after
// the call branches to the try_table's landing pad, which matches the catch
// clauses against thrownTag in compiled code and rethrows if none matches.
// Returns false if no frame in this segment can catch (the caller then
// panics, uncaught).
//
// The frames between the throw site and the catching frame are abandoned in
// place. Their epilogues never run, so the callee-saved registers they used
// are not restored; the compiler accounts for that by keeping nothing in a
// register across a call that can throw inside a try_table, and by saving
// every callee-saved register in such a function's prologue (see
// ssa.Instruction.MarkMayThrow).
func (c *callEngine) handleThrow(exn *wasm.Exception) bool {
	eng := c.parent.parent.parent
	resume := func(fr nativeapi.ThrowFrame, cm *compiledModule) bool {
		fnIdx := cm.functionIndexOf(fr.ReturnAddress)
		entries := cm.ehTables[fnIdx]
		for i := range entries {
			e := &entries[i]
			if fr.ReturnAddress < e.StartOffset || fr.ReturnAddress >= e.EndOffset {
				continue
			}
			c.pendingException = exn
			if len(exn.Params) > 0 {
				c.execCtx.exceptionParamsPtr = uintptr(unsafe.Pointer(&exn.Params[0]))
			}
			// catch_ref/catch_all_ref read this as the exnref they push, and a landing pad with no matching
			// clause rethrows it. A handle, for the same reason the throw path returns one: guest code may keep
			// it past this call. See wasm.ExceptionTable.
			c.execCtx.exceptionRef = uintptr(c.callerModuleInstance().ExceptionTable().Ref(exn))
			c.execCtx.thrownTag = uintptr(unsafe.Pointer(exn.Tag))
			c.execCtx.caughtExceptionClauseIdx = 1
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			sp, fp := resolveThrowTransferSPFP(fr, cm.functionFrameSizes[fnIdx])
			restoreFn := cm.sharedFunctions.throwTransferRegisterRestoreAddress
			afterThrowTransferEntrypointAsm(restoreFn, c.execCtxPtr, sp, fp, fr.ReturnAddress)
			return true
		}
		return false
	}

	frame0 := nativeapi.ThrowFrame{
		ReturnAddress: uintptr(unsafe.Pointer(c.execCtx.goCallReturnAddress)),
		SP:            uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall)),
		FP:            c.execCtx.framePointerBeforeGoCall,
	}
	if cm0 := eng.compiledModuleOfAddr(frame0.ReturnAddress); cm0 != nil && resume(frame0, cm0) {
		return true
	}
	for _, fr := range unwindStackForThrow(frame0.SP, frame0.FP, c.stackTop, nil) {
		cm := eng.compiledModuleOfAddr(fr.ReturnAddress)
		if cm == nil {
			// Reached the trampoline/entry preamble (or, in principle,
			// unknown code): end of this native-stack segment. A throw
			// cannot cross this boundary -- the segment's own
			// callWithStack recover() degrades it to a Go error at the
			// host boundary.
			break
		}
		if resume(fr, cm) {
			return true
		}
	}
	return false
}

func (c *callEngine) callerModuleInstance() *wasm.ModuleInstance {
	return moduleInstanceFromOpaquePtr(c.execCtx.callerModuleContextPtr)
}

const callStackCeiling = uintptr(50000000) // in uint64 (8 bytes) == 400000000 bytes in total == 400mb.

func (c *callEngine) growStackWithGuarded() (newSP uintptr, newFP uintptr, err error) {
	if nativeapi.StackGuardCheckEnabled {
		nativeapi.CheckStackGuardPage(c.stack)
	}
	reserve := c.rootsReserveInUse()
	newSP, newFP, err = c.growStack()
	if err != nil {
		return
	}
	if nativeapi.StackGuardCheckEnabled {
		c.execCtx.stackBottomPtr = c.stackCheckLimitPtr(nativeapi.StackGuardCheckGuardPageSize, reserve)
	}
	return
}

// growStack grows the stack, and returns the new stack pointer.
func (c *callEngine) growStack() (newSP, newFP uintptr, err error) {
	currentLen := uintptr(len(c.stack))
	if callStackCeiling < currentLen {
		err = wasmruntime.ErrRuntimeStackOverflow
		return
	}

	newLen := 2*currentLen + c.execCtx.stackGrowRequiredSize + 16 // Stack might be aligned to 16 bytes, so add 16 bytes just in case.

	// Source the new buffer from the stack pool rather than make()-ing it fresh,
	// so a deep-growth guest reuses the buffers released by callWithStack's grow
	// loop (growStack doubles, so without this every intermediate buffer of the
	// doubling sequence is allocated once and discarded -- 2.12 GB across the
	// spec suite's stack-overflow tests). acquireStack returns a buffer of the
	// smallest class >= newLen; the extra headroom is harmless. Guard-check
	// builds keep make() -- their buffers are never pooled (CheckStackGuardPage
	// needs the guard region to still be all-zero, which a reused buffer can't
	// promise), matching init's debug path.
	var newStack []byte
	if nativeapi.StackGuardCheckEnabled {
		newStack = platform.MakeStack(int(newLen))
	} else {
		newStack, _ = c.eng.acquireStack(int(newLen))
	}
	reserve := c.rootsReserveInUse()
	newSP, newFP, c.stackTop = c.cloneStackInto(newStack)
	c.stack = newStack
	c.execCtx.stackBottomPtr = c.stackCheckLimitPtr(0, reserve)
	return
}

// cloneStack allocates a fresh buffer of length l and copies the live stack
// into it. Used by the experimental Snapshotter (doRestore's counterpart),
// which retains the returned buffer long-term and so must NOT draw it from the
// transient stack pool; growStack instead sources its buffer from the pool and
// calls cloneStackInto directly.
func (c *callEngine) cloneStack(l uintptr) (newSP, newFP, newTop uintptr, newStack []byte) {
	newStack = platform.MakeStack(int(l))
	newSP, newFP, newTop = c.cloneStackInto(newStack)
	return
}

// cloneStackInto copies the live stack into newStack (already allocated, and at
// least the current stack's used size) and returns the relocated SP/FP/top.
func (c *callEngine) cloneStackInto(newStack []byte) (newSP, newFP, newTop uintptr) {
	relSp := c.stackTop - uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall))
	relFp := c.stackTop - c.execCtx.framePointerBeforeGoCall

	// Copy the existing contents in the previous Go-allocated stack into the new
	// one. The stack pointers are raw uintptr addresses, so we build the []byte
	// views via sliceHeader (see hostmodule.go) rather than unsafe.Slice, keeping
	// go vet's unsafeptr check quiet and matching the original semantics.
	var prevStackAligned, newStackAligned []byte
	{
		sh := (*sliceHeader)(unsafe.Pointer(&prevStackAligned))
		sh.Data = c.stackTop - relSp
		sh.Len = int(relSp)
		sh.Cap = int(relSp)
	}
	newTop = alignedStackTop(newStack)
	{
		newSP = newTop - relSp
		newFP = newTop - relFp
		sh := (*sliceHeader)(unsafe.Pointer(&newStackAligned))
		sh.Data = newSP
		sh.Len = int(relSp)
		sh.Cap = int(relSp)
	}
	copy(newStackAligned, prevStackAligned)
	return
}

func (c *callEngine) stackIterator(onHostCall bool) api.StackIterator {
	c.stackIteratorImpl.reset(c, onHostCall)
	return &c.stackIteratorImpl
}

// stackIterator implements api.StackIterator.
type stackIterator struct {
	retAddrs      []uintptr
	retAddrCursor int
	eng           *engine
	pc            uint64

	currentDef *wasm.FunctionDefinition
}

func (si *stackIterator) reset(c *callEngine, onHostCall bool) {
	if onHostCall {
		si.retAddrs = append(si.retAddrs[:0], uintptr(unsafe.Pointer(c.execCtx.goCallReturnAddress)))
	} else {
		si.retAddrs = si.retAddrs[:0]
	}
	si.retAddrs = unwindStack(uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall)), c.execCtx.framePointerBeforeGoCall, c.stackTop, si.retAddrs)
	si.retAddrs = si.retAddrs[:len(si.retAddrs)-1] // the last return addr is the trampoline, so we skip it.
	si.retAddrCursor = 0
	si.eng = c.parent.parent.parent
}

// Next implements the same method as documented on api.StackIterator.
func (si *stackIterator) Next() bool {
	if si.retAddrCursor >= len(si.retAddrs) {
		return false
	}

	addr := si.retAddrs[si.retAddrCursor]
	cm := si.eng.compiledModuleOfAddr(addr)
	if cm != nil {
		index := cm.functionIndexOf(addr)
		def := cm.module.FunctionDefinition(cm.module.ImportFunctionCount + index)
		si.currentDef = def
		si.retAddrCursor++
		si.pc = uint64(addr)
		return true
	}
	return false
}

// ProgramCounter implements the same method as documented on api.StackIterator.
func (si *stackIterator) ProgramCounter() api.ProgramCounter {
	return api.ProgramCounter(si.pc)
}

// Function implements the same method as documented on api.StackIterator.
func (si *stackIterator) Function() api.InternalFunction {
	return si
}

// Definition implements the same method as documented on api.InternalFunction.
func (si *stackIterator) Definition() api.FunctionDefinition {
	return si.currentDef
}

// SourceOffsetForPC implements the same method as documented on api.InternalFunction.
func (si *stackIterator) SourceOffsetForPC(pc api.ProgramCounter) uint64 {
	upc := uintptr(pc)
	cm := si.eng.compiledModuleOfAddr(upc)
	return cm.getSourceOffset(upc)
}

// snapshot implements api.Snapshot
type snapshot struct {
	sp, fp, top    uintptr
	returnAddress  *byte
	stack          []byte
	savedRegisters [savedRegistersLen][2]uint64
	ret            []uint64
	c              *callEngine
}

// Snapshot implements the same method as documented on api.Snapshotter.
func (c *callEngine) Snapshot() api.Snapshot {
	returnAddress := c.execCtx.goCallReturnAddress
	oldTop, oldSp := c.stackTop, uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall))
	newSP, newFP, newTop, newStack := c.cloneStack(uintptr(len(c.stack)) + 16)
	adjustClonedStack(oldSp, oldTop, newSP, newFP, newTop)
	return &snapshot{
		sp:             newSP,
		fp:             newFP,
		top:            newTop,
		savedRegisters: c.execCtx.savedRegisters,
		returnAddress:  returnAddress,
		stack:          newStack,
		c:              c,
	}
}

// Restore implements the same method as documented on api.Snapshot.
func (s *snapshot) Restore(ret []uint64) {
	s.ret = ret
	panic(s)
}

func (s *snapshot) doRestore() {
	spp := *(**uint64)(unsafe.Pointer(&s.sp))
	view := goCallStackView(spp)
	copy(view, s.ret)

	c := s.c
	// The snapshot was taken in this call, whose region it was sized with.
	reserve := c.rootsReserveInUse()
	c.stack = s.stack
	c.stackTop = s.top
	ec := &c.execCtx
	ec.stackBottomPtr = c.stackCheckLimitPtr(0, reserve)
	ec.stackPointerBeforeGoCall = spp
	ec.framePointerBeforeGoCall = s.fp
	ec.goCallReturnAddress = s.returnAddress
	ec.savedRegisters = s.savedRegisters
}

// Error implements the same method on error.
func (s *snapshot) Error() string {
	return "unhandled snapshot restore, this generally indicates restore was called from a different " +
		"exported function invocation than snapshot"
}

func snapshotRecoverFn(c *callEngine) {
	if r := recover(); r != nil {
		if s, ok := r.(*snapshot); ok && s.c == c {
			s.doRestore()
		} else {
			panic(r)
		}
	}
}

// callGoFunctionWithSnapshotRecover calls f.Call, guarded by a defer/recover
// that catches this call engine's own snapshot restores (see snapshot.Restore
// and snapshotRecoverFn). This is only invoked when the snapshotter is
// enabled for the current call; the defer disqualifies this function itself
// from inlining, but keeping it separate lets the (overwhelmingly common)
// snapshotter-disabled path call f.Call directly and inline.
func callGoFunctionWithSnapshotRecover(c *callEngine, ctx context.Context, f api.GoFunction, stack []uint64) {
	defer snapshotRecoverFn(c)
	f.Call(ctx, stack)
}

// callGoModuleFunctionWithSnapshotRecover is callGoFunctionWithSnapshotRecover
// for api.GoModuleFunction, which additionally takes the caller's api.Module.
func callGoModuleFunctionWithSnapshotRecover(c *callEngine, ctx context.Context, f api.GoModuleFunction, mod api.Module, stack []uint64) {
	defer snapshotRecoverFn(c)
	f.Call(ctx, mod, stack)
}

// ScanGCRoots implements wasm.GCRoots.
//
// One conservative walk of the wasm stack covers everything, because a safepoint spills every live wasm value
// into the reserved region at the bottom of that same stack before it parks (see gcRootsReserve). Spilling is
// what makes the scan exact enough: working out where the backend chose to keep a value does not survive a
// change of register allocator -- with more registers to play with, arm64 keeps a loop's live reference in one
// across the safepoint call, where it appears in neither the stack nor the saved registers, which is how a
// reachable object came to be swept there while amd64, forced to spill, was fine.
//
// Words that are not references, and stale ones left by an earlier safepoint, are visited too. That costs a
// failed lookup: a handle carries the generation of the slot it names, so a word that no longer names a live
// object matches nothing.
func (c *callEngine) ScanGCRoots(visit func(uint64)) (complete bool) {
	complete = true
	if len(c.stack) == 0 {
		return
	}
	bottom := uintptr(unsafe.Pointer(&c.stack[0]))
	// Walk down from stackTop rather than up from the slice, because that is the grid the values are on:
	// the stack grows down from an address aligned to 16 bytes *inside* the buffer (see alignedStackTop),
	// so a word starts at stackTop-8k, not at an offset of &stack[0].
	top := int(c.stackTop - bottom)
	if top > len(c.stack) {
		top = len(c.stack)
	}

	// The spilled roots are read exactly, by count, and the walk below stops above them. Reading them
	// conservatively along with everything else would retain what an *earlier* safepoint spilled: the region
	// is written from the bottom up and reused, so a safepoint holding fewer values than one before it leaves
	// the difference behind.
	floor := 0
	if c.parent != nil && c.parent.gcRootsReserve != nil && c.execCtx.stackBottomPtr != nil {
		floor = int(uintptr(unsafe.Pointer(c.execCtx.stackBottomPtr))-bottom) - backend.StackBoundsCheckMarginBytes
		if floor >= 8 && floor <= len(c.stack) {
			hdr := binary.LittleEndian.Uint64(c.stack[floor-8:])
			n := uint32(hdr)
			if n == nativeapi.GCRootsMissing {
				complete = false
				n = 0
			}
			// Never trust a count read out of a buffer the guest could have influenced.
			n = min(n, uint32(hdr>>32), uint32((floor-8)/8))
			for i := 0; i < int(n); i++ {
				visit(binary.LittleEndian.Uint64(c.stack[floor-16-8*i:]))
			}
		} else {
			floor = 0
		}
	}

	for off := top - 8; off >= floor; off -= 8 {
		visit(binary.LittleEndian.Uint64(c.stack[off:]))
	}
	for i := range c.execCtx.savedRegisters {
		visit(c.execCtx.savedRegisters[i][0])
		visit(c.execCtx.savedRegisters[i][1])
	}
	// The exception last thrown or caught: a handler reads its params and exnref from here rather than from
	// the stack, so they are roots until it has.
	visit(uint64(c.execCtx.exceptionRef))
	if exn := c.pendingException; exn != nil {
		for _, w := range exn.Params {
			visit(w)
		}
	}
	return
}
