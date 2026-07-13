package native

import (
	"context"
	"fmt"
	"runtime"
	"sync/atomic"
	"unsafe"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/experimental"
	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
	"github.com/samyfodil/wazy/internal/expctxkeys"
	"github.com/samyfodil/wazy/internal/internalapi"
	"github.com/samyfodil/wazy/internal/wasm"
	"github.com/samyfodil/wazy/internal/wasmdebug"
	"github.com/samyfodil/wazy/internal/wasmruntime"
)

type (
	// callEngine implements api.Function.
	callEngine struct {
		internalapi.WazyOnly
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
		// indexInModule is the index of the function in the module.
		indexInModule wasm.Index
		// sizeOfParamResultSlice is the size of the parameter/result slice.
		sizeOfParamResultSlice int
		requiredParams         int
		// execCtx holds various information to be read/written by assembly functions.
		execCtx executionContext
		// execCtxPtr holds the pointer to the executionContext which doesn't change after callEngine is created.
		execCtxPtr        uintptr
		numberOfResults   int
		stackIteratorImpl stackIterator
		// activeCatchScopes is the stack of currently-open try_table (with
		// catch clauses) activations, pushed/popped 1:1 by
		// ExitCodeTryTableEnter/Leave exactly as tryHandlers was before,
		// but slimmed to only what a throw can no longer recompute from the
		// exception side table itself: which module instance owns this
		// scope's tag namespace and where its locals save area lives.
		// Catch-clause matching and control transfer are driven by the
		// per-function exception side table (compiledModule.ehTables) plus
		// a native stack walk (see (*callEngine).handleThrow), not by
		// searching this slice -- it exists purely to recover, for
		// whichever frame turns out to catch, the two pieces of state that
		// are NOT recoverable from the (unwound, but never cloned/rolled
		// back) native stack alone.
		activeCatchScopes []activeCatchScope
		// ehLocalsPool holds one reusable locals-mirror buffer per
		// currently-open *non-nested* try_table-with-catch scope (see
		// acquireLocalsSaveArea), indexed by ehLocalsPoolDepth's value at
		// the time the buffer was acquired. Nested same-function try_tables
		// (TryTableInfo.ReuseLocals) share their enclosing scope's buffer
		// and never touch this pool. Buffers are reused -- grown via make
		// only when a given depth needs more locals than its pool entry
		// currently holds -- across dynamic enters *and* across separate
		// calls to this callEngine, so a hot loop that repeatedly
		// enters/leaves a try_table at the same nesting depth allocates at
		// most once (on the first, capacity-establishing entry) rather than
		// on every entry. This is what replaces the old
		// make([]uint64, NumLocals*2) done unconditionally by
		// ExitCodeTryTableEnter.
		ehLocalsPool [][]uint64
		// ehLocalsPoolDepth is the number of currently-open non-nested
		// try_table-with-catch scopes (i.e. scopes that own -- rather than
		// share -- a locals-mirror buffer), and the index into ehLocalsPool
		// that the *next* such scope will acquire. Reset to 0 at the start
		// of every call; decremented in lockstep with activeCatchScopes
		// truncation wherever a scope owning a buffer is popped (see
		// popCatchScopes).
		ehLocalsPoolDepth int
		// pendingException holds the most recently caught exception, so handler
		// code can read its params after re-entry.
		pendingException *wasm.Exception
	}

	// activeCatchScope records the state a throw cannot recover merely by
	// unwinding the native stack: which module instance to resolve
	// catch-clause tag indices against, where the throw-time locals live,
	// and a callee-saved-register snapshot. Pushed/popped in exact lockstep
	// with try_table enter/leave (ExitCodeTryTableEnter/Leave below), one
	// entry per dynamic try_table-with-catch entry -- identical cadence to
	// the old tryHandler, just without the fields the side table replaced
	// (sp/fp/top/returnAddress/stack/catchClauses): catch-clause matching
	// and the SP/FP to resume at are now derived from compiledModule.ehTables
	// and a fresh stack walk at throw time instead of a stored checkpoint.
	//
	// savedRegisters is still captured and restored *unconditionally* for
	// every try_table-with-catch scope (this MVP pass tried gating it on
	// TryTableInfo.FloorSize == 0 -- an empty wasm operand floor -- and
	// reverted that: a real C++/Emscripten-compiled module crashed, because
	// handler blocks unconditionally call reloadAfterCall(), which reads
	// through moduleCtxPtrValue -- a per-function SSA value live across the
	// *entire* function body, regardless of the wasm operand stack, that
	// regalloc is free to keep in a callee-saved register across the enter
	// call exactly like a category-4 value. See TryTableInfo.FloorSize's
	// doc comment and the longer note at the ExitCodeTryTableEnter case
	// below for the full story; this is the top item left for Opus). The
	// throw-time control transfer resumes the catching frame directly at
	// its try_table enter-continuation (resumePC below), bypassing the
	// enter trampoline's own register-restore tail that the non-throwing
	// path returns through. Any value the catching function's own code
	// keeps live across the try body in a callee-saved register (which the
	// register allocator does freely, e.g. for a value used both before and
	// after a call inside the body -- moduleCtxPtrValue being the
	// pervasive, whole-function-lived example) would otherwise read back
	// whatever the deepest abandoned callee last left there. Restoring this
	// snapshot -- captured cheaply here (a 1KB struct copy done by the
	// enter trampoline's own prologue via saveRegistersInExecutionContext,
	// not a stack clone) -- via afterThrowTransferEntrypoint before the
	// jump keeps that correct without any frontend/regalloc changes. See
	// backend.Machine.CompileThrowTransferRegisterRestore.
	activeCatchScope struct {
		// moduleInstance is the module that set up this scope. Used for tag
		// matching (the tag index in catch clauses is relative to this
		// module's tag index space) exactly as tryHandler.moduleInstance was.
		moduleInstance *wasm.ModuleInstance
		// resumePC is the native return address of this try_table's enter
		// trampoline call: the instruction the non-throwing path resumes at
		// with caughtExceptionClauseIdx == -1, i.e. the reload+br_table the
		// backend compiled right after the enter call. A throw resumes the
		// catching frame at exactly this PC (with caughtExceptionClauseIdx
		// set to the matched clause instead of -1) so the compiled
		// br_table dispatches to the handler block with every
		// register/reload invariant (execCtx reload, savedRegisters
		// restored) satisfied identically to the fast path -- rather than
		// jumping straight into a raw landing pad, which on arm64 would
		// bypass the execCtx reload the handler inherits from this exact
		// point (see (*callEngine).handleThrow and afterThrowTransferEntrypoint).
		// It is a code address into the (mmap'd, never-relocated)
		// executable kept alive by the compiledModule, so it is both
		// immune to stack growth/relocation between enter and throw and
		// safe to hold as a bare uintptr for the duration of the call.
		// Captured for free from the enter trampoline's own frame
		// (firstReturnAddress) at enter time.
		resumePC uintptr
		// savedRegisters is a copy of execCtx.savedRegisters at the moment
		// this try_table was entered (i.e. already captured, for free, by
		// the enter trampoline's own prologue -- see
		// saveRegistersInExecutionContext -- before any Go code runs).
		// Captured unconditionally for every scope; see this struct's own
		// doc comment above for why it cannot yet be made conditional.
		savedRegisters [64][2]uint64
		// localsSaveArea is this scope's locals-mirror buffer -- acquired
		// from callEngine.ehLocalsPool (acquireLocalsSaveArea), NOT a fresh
		// heap allocation. Handlers read from it to get throw-time values.
		// Nil for nested same-function try_tables that reuse the enclosing
		// scope's save area (TryTableInfo.ReuseLocals) and for try_tables
		// whose function has no locals.
		localsSaveArea []uint64
	}

	// executionContext is the struct to be read/written by assembly functions.
	executionContext struct {
		// exitCode holds the nativeapi.ExitCode describing the state of the function execution.
		exitCode nativeapi.ExitCode
		// callerModuleContextPtr holds the moduleContextOpaque for Go function calls.
		callerModuleContextPtr *byte
		// originalFramePointer holds the original frame pointer of the caller of the assembly function.
		originalFramePointer uintptr
		// originalStackPointer holds the original stack pointer of the caller of the assembly function.
		originalStackPointer uintptr
		// goReturnAddress holds the return address to go back to the caller of the assembly function.
		goReturnAddress uintptr
		// stackBottomPtr holds the pointer to the bottom of the stack.
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
		// We want to align 16 bytes for each register, so we use [64][2]uint64.
		savedRegisters [64][2]uint64
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
		// tryTableEnterTrampolineAddress holds the address of the try_table enter trampoline function.
		tryTableEnterTrampolineAddress *byte
		// tryTableLeaveTrampolineAddress holds the address of the try_table leave trampoline function.
		tryTableLeaveTrampolineAddress *byte
		// exceptionPtr holds the pointer to the Exception struct,
		// used on the throw side (throwAlloc stores the new Exception)
		// and on the catch side (catch_ref/catch_all_ref retrieve the exnref).
		exceptionPtr uintptr
		// exceptionParamsPtr points into exceptionPtr's Params slice
		// backing array. On the throw side, throwAlloc sets it so compiled
		// code can store params at [ptr + i*8]. On the catch side, compiled
		// handler blocks load params from the same pointer.
		exceptionParamsPtr uintptr
		// caughtExceptionClauseIdx is set by the dispatch loop to -1 on
		// TryTableEnter (normal path) or to the matched catch clause index
		// when an exception is caught. Compiled code loads this from execCtx
		// after the trampoline call to decide which handler to dispatch to.
		caughtExceptionClauseIdx int64
		// localsSaveAreaPtr points to the tryHandler's localsSaveArea slice
		// backing array. Handlers load locals from this slice.
		localsSaveAreaPtr uintptr
	}
)

func (c *callEngine) requiredInitialStackSize() int {
	// stackPoolBaseSize (stack_pool.go) is this function's own default, kept
	// as a single source of truth since it is also pool size class 0's
	// canonical size -- see that file's doc comment.
	stackSize := stackPoolBaseSize
	paramResultInBytes := c.sizeOfParamResultSlice * 8 * 2 // * 8 because uint64 is 8 bytes, and *2 because we need both separated param/result slots.
	required := paramResultInBytes + 32 + 16               // 32 is enough to accommodate the call frame info, and 16 exists just in case when []byte is not aligned to 16 bytes.
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
// call returns.
//
// It can't just be done once here instead of in callWithStack: a single
// callEngine can have Call/CallWithStack invoked more than once across its
// lifetime -- e.g. a cached api.Function handle reused in a hot loop, as
// BenchmarkInvocation's fib benchmarks and BenchmarkRedundancyHeavyExec
// both do -- and the pool's contract is "acquire at the start of a
// top-level call, release when it returns". Acquiring only once here and
// releasing after the first call returns would hand this callEngine's
// buffer to a different pool consumer while a second call on the very same
// callEngine still thinks it owns it.
func (c *callEngine) init() {
	if nativeapi.StackGuardCheckEnabled {
		// Debug-only path: never pooled -- see acquireStack's doc comment
		// for why (in short: CheckStackGuardPage requires the guard page
		// to still be all-zero, which a reused buffer cannot guarantee) --
		// so it keeps the simple eager-allocate-once-here behavior.
		stackSize := c.requiredInitialStackSize() + nativeapi.StackGuardCheckGuardPageSize
		c.stack = make([]byte, stackSize)
		c.stackTop = alignedStackTop(c.stack)
		c.execCtx.stackBottomPtr = &c.stack[nativeapi.StackGuardCheckGuardPageSize]
	}
	c.execCtxPtr = uintptr(unsafe.Pointer(&c.execCtx))
}

// alignedStackTop returns 16-bytes aligned stack top of given stack.
// 16 bytes should be good for all platform (arm64/amd64).
func alignedStackTop(s []byte) uintptr {
	stackAddr := uintptr(unsafe.Pointer(&s[len(s)-1]))
	return stackAddr - (stackAddr & (16 - 1))
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

func (c *callEngine) addFrame(builder wasmdebug.ErrorBuilder, addr uintptr) (def api.FunctionDefinition, listener experimental.FunctionListener) {
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
		defer func() {
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
		var stackBoxed *[]byte
		c.stack, stackBoxed = eng.acquireStack(c.requiredInitialStackSize())
		c.stackTop = alignedStackTop(c.stack)
		c.execCtx.stackBottomPtr = &c.stack[0]
		defer func() {
			// Release whichever buffer c.stack currently is -- not
			// necessarily the one just acquired above: stack growth
			// (growStack/cloneStack) or an experimental Snapshot restore
			// (doRestore) may have replaced it with a different buffer
			// mid-call. Exactly one buffer is released here, exactly once,
			// matching the exactly-one acquire above; any old,
			// pre-growth/pre-restore buffer is simply not returned to the
			// pool and left for the GC, same as before pooling existed.
			eng.releaseStack(c.stack, stackBoxed)
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

	// Clear any stale try_table handler state from a previous call. Note:
	// ehLocalsPool itself is intentionally NOT cleared -- its whole purpose
	// is to be reused across calls (see its doc comment); only the
	// per-call depth counter resets.
	c.activeCatchScopes = c.activeCatchScopes[:0]
	c.ehLocalsPoolDepth = 0

	var paramResultPtr *uint64
	if len(paramResultStack) > 0 {
		paramResultPtr = &paramResultStack[0]
	}
	defer func() {
		r := recover()
		if s, ok := r.(*snapshot); ok {
			// A snapshot that wasn't handled was created by a different call engine possibly from a nested wasm invocation,
			// let it propagate up to be handled by the caller.
			panic(s)
		}
		if r != nil {
			type listenerForAbort struct {
				def api.FunctionDefinition
				lsn experimental.FunctionListener
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
		} else {
			if err != wasmruntime.ErrRuntimeStackOverflow { // Stackoverflow case shouldn't be panic (to avoid extreme stack unwinding).
				err = c.parent.module.FailIfClosed()
			}
		}

		if err != nil {
			// Ensures that we can reuse this callEngine even after an error.
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			c.activeCatchScopes = c.activeCatchScopes[:0]
			c.ehLocalsPoolDepth = 0
		}
	}()

	if ensureTermination {
		done := m.CloseModuleOnCanceledOrTimeout(ctx)
		defer done()
	}

	if c.stackTop&(16-1) != 0 {
		panic("BUG: stack must be aligned to 16 bytes")
	}
	entrypoint(c.preambleExecutable, c.executable, c.execCtxPtr, c.parent.opaquePtr, paramResultPtr, c.stackTop)
	for {
		switch ec := c.execCtx.exitCode; ec & nativeapi.ExitCodeMask {
		case nativeapi.ExitCodeOK:
			return nil
		case nativeapi.ExitCodeGrowStack:
			oldsp := uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall))
			oldTop := c.stackTop
			oldStack := c.stack
			var newsp, newfp uintptr
			if nativeapi.StackGuardCheckEnabled {
				newsp, newfp, err = c.growStackWithGuarded()
			} else {
				newsp, newfp, err = c.growStack()
			}
			if err != nil {
				return err
			}
			adjustClonedStack(oldsp, oldTop, newsp, newfp, c.stackTop)
			// Old stack must be alive until the new stack is adjusted.
			runtime.KeepAlive(oldStack)
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			afterGoFunctionCallEntrypoint(c.execCtx.goCallReturnAddress, c.execCtxPtr, newsp, newfp)
		case nativeapi.ExitCodeGrowMemory:
			mod := c.callerModuleInstance()
			mem := mod.MemoryInstance
			s := goCallStackView(c.execCtx.stackPointerBeforeGoCall)
			argRes := &s[0]
			if res, ok := mem.Grow(uint32(*argRes)); !ok {
				*argRes = uint64(0xffffffff) // = -1 in signed 32-bit integer.
			} else {
				*argRes = uint64(res)
			}
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			afterGoFunctionCallEntrypoint(c.execCtx.goCallReturnAddress, c.execCtxPtr, uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall)), c.execCtx.framePointerBeforeGoCall)
		case nativeapi.ExitCodeTableGrow:
			mod := c.callerModuleInstance()
			s := goCallStackView(c.execCtx.stackPointerBeforeGoCall)
			tableIndex, num, ref := uint32(s[0]), uint32(s[1]), uintptr(s[2])
			table := mod.Tables[tableIndex]
			s[0] = uint64(uint32(int32(table.Grow(num, ref))))
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			afterGoFunctionCallEntrypoint(c.execCtx.goCallReturnAddress, c.execCtxPtr,
				uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall)), c.execCtx.framePointerBeforeGoCall)
		case nativeapi.ExitCodeCallGoFunction:
			index := nativeapi.GoFunctionIndexFromExitCode(ec)
			f := hostModuleGoFuncFromOpaque[api.GoFunction](index, c.execCtx.goFunctionCallCalleeModuleContextOpaque)
			stack := goCallStackView(c.execCtx.stackPointerBeforeGoCall)
			if snapshotEnabled {
				callGoFunctionWithSnapshotRecover(c, ctx, f, stack)
			} else {
				f.Call(ctx, stack)
			}
			// Back to the native code.
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			afterGoFunctionCallEntrypoint(c.execCtx.goCallReturnAddress, c.execCtxPtr,
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
			// Call into the Go function.
			if snapshotEnabled {
				callGoFunctionWithSnapshotRecover(c, ctx, f, s)
			} else {
				f.Call(ctx, s)
			}
			// Call Listener.After.
			listener.After(ctx, callerModule, def, s)
			// Back to the native code.
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			afterGoFunctionCallEntrypoint(c.execCtx.goCallReturnAddress, c.execCtxPtr,
				uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall)), c.execCtx.framePointerBeforeGoCall)
		case nativeapi.ExitCodeCallGoModuleFunction:
			index := nativeapi.GoFunctionIndexFromExitCode(ec)
			f := hostModuleGoFuncFromOpaque[api.GoModuleFunction](index, c.execCtx.goFunctionCallCalleeModuleContextOpaque)
			mod := c.callerModuleInstance()
			stack := goCallStackView(c.execCtx.stackPointerBeforeGoCall)
			if snapshotEnabled {
				callGoModuleFunctionWithSnapshotRecover(c, ctx, f, mod, stack)
			} else {
				f.Call(ctx, mod, stack)
			}
			// Back to the native code.
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			afterGoFunctionCallEntrypoint(c.execCtx.goCallReturnAddress, c.execCtxPtr,
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
			// Call into the Go function.
			if snapshotEnabled {
				callGoModuleFunctionWithSnapshotRecover(c, ctx, f, callerModule, s)
			} else {
				f.Call(ctx, callerModule, s)
			}
			// Call Listener.After.
			listener.After(ctx, callerModule, def, s)
			// Back to the native code.
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			afterGoFunctionCallEntrypoint(c.execCtx.goCallReturnAddress, c.execCtxPtr,
				uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall)), c.execCtx.framePointerBeforeGoCall)
		case nativeapi.ExitCodeCallListenerBefore:
			stack := goCallStackView(c.execCtx.stackPointerBeforeGoCall)
			index := wasm.Index(stack[0])
			mod := c.callerModuleInstance()
			listener := mod.Engine.(*moduleEngine).listeners[index]
			def := mod.Source.FunctionDefinition(index + mod.Source.ImportFunctionCount)
			listener.Before(ctx, mod, def, stack[1:], c.stackIterator(false))
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			afterGoFunctionCallEntrypoint(c.execCtx.goCallReturnAddress, c.execCtxPtr,
				uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall)), c.execCtx.framePointerBeforeGoCall)
		case nativeapi.ExitCodeCallListenerAfter:
			stack := goCallStackView(c.execCtx.stackPointerBeforeGoCall)
			index := wasm.Index(stack[0])
			mod := c.callerModuleInstance()
			listener := mod.Engine.(*moduleEngine).listeners[index]
			def := mod.Source.FunctionDefinition(index + mod.Source.ImportFunctionCount)
			listener.After(ctx, mod, def, stack[1:])
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			afterGoFunctionCallEntrypoint(c.execCtx.goCallReturnAddress, c.execCtxPtr,
				uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall)), c.execCtx.framePointerBeforeGoCall)
		case nativeapi.ExitCodeCheckModuleExitCode:
			// Note: this operation must be done in Go, not native code. The reason is that
			// native code cannot be preempted and that means it can block forever if there are not
			// enough OS threads (which we don't have control over).
			if err := m.FailIfClosed(); err != nil {
				panic(err)
			}
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			afterGoFunctionCallEntrypoint(c.execCtx.goCallReturnAddress, c.execCtxPtr,
				uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall)), c.execCtx.framePointerBeforeGoCall)
		case nativeapi.ExitCodeRefFunc:
			mod := c.callerModuleInstance()
			s := goCallStackView(c.execCtx.stackPointerBeforeGoCall)
			funcIndex := wasm.Index(s[0])
			ref := mod.Engine.FunctionInstanceReference(funcIndex)
			s[0] = uint64(ref)
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			afterGoFunctionCallEntrypoint(c.execCtx.goCallReturnAddress, c.execCtxPtr,
				uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall)), c.execCtx.framePointerBeforeGoCall)
		case nativeapi.ExitCodeMemoryWait32:
			mod := c.callerModuleInstance()
			mem := mod.MemoryInstance
			if !mem.Shared {
				panic(wasmruntime.ErrRuntimeExpectedSharedMemory)
			}

			s := goCallStackView(c.execCtx.stackPointerBeforeGoCall)
			timeout, exp, addr := int64(s[0]), uint32(s[1]), uintptr(s[2])
			base := uintptr(unsafe.Pointer(&mem.Buffer[0]))

			offset := uint32(addr - base)
			res := mem.Wait32(offset, exp, timeout, func(mem *wasm.MemoryInstance, offset uint32) uint32 {
				addr := unsafe.Add(unsafe.Pointer(&mem.Buffer[0]), offset)
				return atomic.LoadUint32((*uint32)(addr))
			})
			s[0] = res
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			afterGoFunctionCallEntrypoint(c.execCtx.goCallReturnAddress, c.execCtxPtr,
				uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall)), c.execCtx.framePointerBeforeGoCall)
		case nativeapi.ExitCodeMemoryWait64:
			mod := c.callerModuleInstance()
			mem := mod.MemoryInstance
			if !mem.Shared {
				panic(wasmruntime.ErrRuntimeExpectedSharedMemory)
			}

			s := goCallStackView(c.execCtx.stackPointerBeforeGoCall)
			timeout, exp, addr := int64(s[0]), uint64(s[1]), uintptr(s[2])
			base := uintptr(unsafe.Pointer(&mem.Buffer[0]))

			offset := uint32(addr - base)
			res := mem.Wait64(offset, exp, timeout, func(mem *wasm.MemoryInstance, offset uint32) uint64 {
				addr := unsafe.Add(unsafe.Pointer(&mem.Buffer[0]), offset)
				return atomic.LoadUint64((*uint64)(addr))
			})
			s[0] = uint64(res)
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			afterGoFunctionCallEntrypoint(c.execCtx.goCallReturnAddress, c.execCtxPtr,
				uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall)), c.execCtx.framePointerBeforeGoCall)
		case nativeapi.ExitCodeMemoryNotify:
			mod := c.callerModuleInstance()
			mem := mod.MemoryInstance

			s := goCallStackView(c.execCtx.stackPointerBeforeGoCall)
			count, addr := uint32(s[0]), s[1]
			offset := uint32(uintptr(addr) - uintptr(unsafe.Pointer(&mem.Buffer[0])))
			res := mem.Notify(offset, count)
			s[0] = uint64(res)
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			afterGoFunctionCallEntrypoint(c.execCtx.goCallReturnAddress, c.execCtxPtr,
				uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall)), c.execCtx.framePointerBeforeGoCall)
		case nativeapi.ExitCodeUnreachable:
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
			c.pendingException = exn // GC root: keeps exn alive while compiled code writes params
			if nParams > 0 {
				c.execCtx.exceptionParamsPtr = uintptr(unsafe.Pointer(&exn.Params[0]))
			}
			// Return the exnref to compiled code via the stack slot.
			s[0] = uint64(uintptr(unsafe.Pointer(exn)))
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			afterGoFunctionCallEntrypoint(c.execCtx.goCallReturnAddress, c.execCtxPtr,
				uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall)), c.execCtx.framePointerBeforeGoCall)
		case nativeapi.ExitCodeThrow:
			// Throw trampoline: (execCtx, exnref) → ().
			// Reads the exnref from the stack, then walks the native stack
			// against the per-function exception side table and transfers
			// control directly to a matching landing pad -- see handleThrow.
			s := goCallStackView(c.execCtx.stackPointerBeforeGoCall)
			// Read the Exception pointer directly from the uint64 value to avoid
			// conversion from uintptr into unsafe.Pointer, which triggers checkptr.
			exn := *(**wasm.Exception)(unsafe.Pointer(&s[0]))
			if !c.handleThrow(exn) {
				panic(wasmruntime.ErrRuntimeUncaughtException)
			}
			// handleThrow already transferred control on a match (the
			// destination PC/SP/FP are the matched landing pad's, not
			// goCallReturnAddress/stackPointerBeforeGoCall like every other
			// exit code's uniform "resume where we left off").
		case nativeapi.ExitCodeNullReference:
			panic(wasmruntime.ErrRuntimeNullReference)
		case nativeapi.ExitCodeTryTableEnter:
			// Push a slimmed activeCatchScope for this try_table. Unlike
			// the old tryHandler this does no stack cloning (the >=10KB
			// clone is gone): catch-clause matching and the resume SP/FP
			// now come from the exception side table and a fresh stack walk
			// at throw time (handleThrow), not from a checkpoint captured
			// here. What must still be recorded per dynamic entry is the
			// state a throw cannot recompute merely by unwinding: which
			// module instance owns this scope's tag namespace, and where
			// its locals mirror buffer (if any) lives. Unlike P1, the
			// locals buffer no longer allocates on the common path: it
			// comes from ehLocalsPool (acquireLocalsSaveArea), reused
			// across dynamic enters instead of make()'d fresh every time.
			// The callee-saved-register snapshot below is still captured
			// unconditionally, not skipped based on TryTableInfo.FloorSize
			// -- see activeCatchScope's doc comment for why (a real
			// C++/Emscripten module crashed when this MVP tried gating it
			// on an empty wasm operand floor: moduleCtxPtrValue's
			// whole-function live range needs the same protection).
			//
			// The encoded exit code (with tryTableID in upper bits) is on the
			// Go call stack as the second trampoline argument, not in execCtx.exitCode.
			tryTableEnterStack := goCallStackView(c.execCtx.stackPointerBeforeGoCall)
			tryTableID := nativeapi.TryTableIDFromExitCode(nativeapi.ExitCode(tryTableEnterStack[0]))
			mod := c.callerModuleInstance()
			me := mod.Engine.(*moduleEngine)
			info := &me.parent.tryTableInfo[tryTableID]

			// Acquire a (reused, not freshly heap-allocated) buffer for
			// locals so handlers can read throw-time values. Nested
			// try_tables in the same function (ReuseLocals) share the
			// enclosing scope's save area and never touch the pool.
			var saveArea []uint64
			if info.NumLocals > 0 && !info.ReuseLocals {
				saveArea = c.acquireLocalsSaveArea(info.NumLocals)
				c.execCtx.localsSaveAreaPtr = uintptr(unsafe.Pointer(&saveArea[0]))
			}

			// Capture this try_table's enter-continuation: the return
			// address of the enter trampoline call, i.e. the reload+br_table
			// the compiled catching frame resumes at once the trampoline
			// returns. c.execCtx.goCallReturnAddress is NOT it (that points
			// into the shared enter trampoline's own restore-and-return
			// tail, which depends on the trampoline's now-transient stack
			// frame); the enter-continuation is the trampoline's caller
			// return address, recoverable in O(1) from the trampoline's
			// still-live frame exactly as the throw-time walk recovers each
			// frame's return address. This is a pure read of an address
			// already on the stack -- no clone, no walk of the whole stack
			// -- so the non-throwing fast path is unchanged.
			resumePC := firstReturnAddress(
				uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall)),
				c.execCtx.framePointerBeforeGoCall, c.stackTop)

			// The callee-saved register snapshot is captured unconditionally
			// (NOT gated on TryTableInfo.FloorSize) -- see this field's own
			// doc comment for why FloorSize alone is not a sufficient
			// condition to skip it: a real C++/Emscripten-compiled module
			// (experimental/exceptions_test.go's TestCppExceptions) crashed
			// when this was gated on an empty wasm operand floor, because
			// handler blocks unconditionally call reloadAfterCall(), which
			// reads through moduleCtxPtrValue -- a per-function SSA value
			// with a live range spanning the *entire* function body,
			// completely independent of the wasm operand stack, that
			// regalloc is free to keep in a callee-saved register across
			// the enter call exactly like any category-4 value. Skipping
			// the restore left that register holding whatever the deepest
			// abandoned callee last wrote there, so any handler that
			// touched memory or a mutable global (i.e. almost every
			// realistic module, just not the memory/global-free synthetic
			// benchmarks) read through a corrupted moduleCtxPtrValue. Left
			// for Opus: give moduleCtxPtrValue (and anything else with a
			// whole-function live range) its own memory-based reload in
			// handler blocks, symmetric to how locals are reloaded, so the
			// register snapshot can be skipped safely and generally rather
			// than always paid for.
			c.activeCatchScopes = append(c.activeCatchScopes, activeCatchScope{
				moduleInstance: mod,
				resumePC:       resumePC,
				savedRegisters: c.execCtx.savedRegisters,
				localsSaveArea: saveArea,
			})
			// Set clauseIdx = -1 (no exception) in execCtx for the compiled code
			// to read after the trampoline returns.
			c.execCtx.caughtExceptionClauseIdx = -1
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			afterGoFunctionCallEntrypoint(c.execCtx.goCallReturnAddress, c.execCtxPtr,
				uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall)), c.execCtx.framePointerBeforeGoCall)
		case nativeapi.ExitCodeTryTableLeave:
			// Pop the most recent scope and restore the locals save area
			// pointer from the scope below (or clear it).
			if n := len(c.activeCatchScopes); n > 0 {
				c.activeCatchScopes = c.popCatchScopes(c.activeCatchScopes, n-1)
				c.restoreLocalsSaveAreaPtrFrom(c.activeCatchScopes, n-2)
			}
			c.execCtx.exitCode = nativeapi.ExitCodeOK
			afterGoFunctionCallEntrypoint(c.execCtx.goCallReturnAddress, c.execCtxPtr,
				uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall)), c.execCtx.framePointerBeforeGoCall)
		default:
			panic("BUG")
		}
	}
}

// handleThrow searches the current native stack segment for a try_table
// whose exception side table entry (compiledModule.ehTables) contains some
// frame's own PC and whose clauses match exn's tag, walking outward from the
// throw site (frame 0) through its ancestors. On a match it transfers
// control directly to that clause's landing pad -- via
// afterGoFunctionCallEntrypoint with the matching frame's own SP/FP,
// recovered fresh from the walk, never captured/rolled back -- and returns
// true. Returns false if no frame in this segment catches it (the caller
// then panics, uncaught, exactly as before).
//
// This replaces the old doHandleException's tryHandlers search plus full
// stack/register checkpoint restore: there is no checkpoint any more (try
// entry clones nothing), so a caught exception simply abandons every frame
// between the throw site and the catching frame in place -- their memory is
// never touched again -- and jumps straight into the catching frame's own,
// still-intact native stack frame, mirroring the interpreter's
// searchExceptionTable/applyExceptionHandler (which also never rolls back
// state, just unwinds).
func (c *callEngine) handleThrow(exn *wasm.Exception) bool {
	eng := c.parent.parent.parent
	scopes := c.activeCatchScopes

	// tryFrame attempts to match exn against fr's own try_tables (cm's
	// ehTable entries containing fr.ReturnAddress, searched innermost
	// first -- entries are recorded in encounter order, so a backward scan
	// mirrors searchExceptionTable's). On a match, it finalizes
	// activeCatchScopes and transfers control, returning true. On no
	// match, it discards whatever fr contributed to `scopes` (so the next,
	// shallower frame's own scopes are back on top) and returns false.
	tryFrame := func(fr nativeapi.ThrowFrame, cm *compiledModule) bool {
		fnIdx := cm.functionIndexOf(fr.ReturnAddress)
		entries := cm.ehTables[fnIdx]

		containing := 0
		for i := len(entries) - 1; i >= 0; i-- {
			e := &entries[i]
			if fr.ReturnAddress < e.StartOffset || fr.ReturnAddress >= e.EndOffset {
				continue
			}
			scopeIdx := len(scopes) - 1 - containing
			if scopeIdx < 0 {
				panic("BUG: activeCatchScopes underflow while matching a throw")
			}
			mod := scopes[scopeIdx].moduleInstance
			for ci := range e.Clauses {
				cl := &e.Clauses[ci]
				matched := cl.Kind == wasm.CatchKindCatchAll || cl.Kind == wasm.CatchKindCatchAllRef ||
					mod.Tags[cl.TagIndex] == exn.Tag
				if !matched {
					continue
				}

				// Restore localsSaveAreaPtr from this scope or the
				// nearest enclosing one that owns a save area
				// (same-function ReuseLocals scopes share their
				// enclosing scope's), and the callee-saved register
				// snapshot from this exact scope -- unconditionally; see
				// activeCatchScope's doc comment for why this cannot yet
				// be gated on TryTableInfo.FloorSize. Then discard this
				// scope and everything above it: their try_tables are
				// being unwound past, exactly as doHandleException used
				// to truncate tryHandlers.
				c.restoreLocalsSaveAreaPtrFrom(scopes, scopeIdx)
				c.execCtx.savedRegisters = scopes[scopeIdx].savedRegisters
				c.activeCatchScopes = c.popCatchScopes(scopes, scopeIdx)

				// The matched clause's index ci within e.Clauses
				// (declaration order) is exactly what the compiled br_table
				// at the enter-continuation dispatches on, so it feeds
				// caughtExceptionClauseIdx directly. cl.LandingPad is no
				// longer used by the transfer -- the br_table, not a direct
				// jump, routes to the handler block -- it survives only as
				// the dead-code marker buildEhEntries uses to drop
				// unreachable try_tables (see nativeapi.EhClause).
				c.pendingException = exn
				if len(exn.Params) > 0 {
					c.execCtx.exceptionParamsPtr = uintptr(unsafe.Pointer(&exn.Params[0]))
				}
				c.execCtx.exceptionPtr = uintptr(unsafe.Pointer(exn))
				// The matched clause index, read by the compiled code at
				// resumePC exactly where the non-throwing path reads -1.
				c.execCtx.caughtExceptionClauseIdx = int64(ci)
				c.execCtx.exitCode = nativeapi.ExitCodeOK

				// Resume the catching frame at its try_table
				// enter-continuation (resumePC) -- the same PC, SP/FP, and
				// restored-register state the non-throwing path resumes with
				// -- rather than jumping into the raw landing pad. The
				// compiled reload+br_table there then establishes execCtx
				// (in x8 on arm64, RBP-relative on amd64) and dispatches to
				// handler ci, identically to the fast path. SP/FP come from
				// the fresh walk (never a stale checkpoint); resumePC is the
				// scope's captured code address.
				sp, fp := resolveThrowTransferSPFP(fr, cm.functionFrameSizes[fnIdx])
				resumePC := scopes[scopeIdx].resumePC
				restoreFn := cm.sharedFunctions.throwTransferRegisterRestoreAddress
				afterThrowTransferEntrypoint(restoreFn, c.execCtxPtr, sp, fp, resumePC)
				return true
			}
			containing++
		}
		scopes = c.popCatchScopes(scopes, len(scopes)-containing)
		return false
	}

	frame0 := nativeapi.ThrowFrame{
		ReturnAddress: uintptr(unsafe.Pointer(c.execCtx.goCallReturnAddress)),
		SP:            uintptr(unsafe.Pointer(c.execCtx.stackPointerBeforeGoCall)),
		FP:            c.execCtx.framePointerBeforeGoCall,
	}
	if cm0 := eng.compiledModuleOfAddr(frame0.ReturnAddress); cm0 != nil && tryFrame(frame0, cm0) {
		return true
	}

	for _, fr := range unwindStackForThrow(frame0.SP, frame0.FP, c.stackTop, nil) {
		cm := eng.compiledModuleOfAddr(fr.ReturnAddress)
		if cm == nil {
			// Reached the trampoline/entry preamble (or, in principle,
			// unknown code): end of this native-stack segment. A throw
			// cannot cross this boundary -- the segment's own
			// callWithStack recover() degrades it to a Go error at the
			// host boundary, exactly as tryHandlers scoping did before.
			break
		}
		if tryFrame(fr, cm) {
			return true
		}
	}
	c.activeCatchScopes = scopes
	return false
}

// restoreLocalsSaveAreaPtrFrom walks scopes from index `from` downward and
// sets execCtx.localsSaveAreaPtr to the first scope that owns a save area
// (nested same-function ReuseLocals scopes share their enclosing scope's),
// or clears it if none is found (including when from < 0, i.e. no
// enclosing scope exists at all).
func (c *callEngine) restoreLocalsSaveAreaPtrFrom(scopes []activeCatchScope, from int) {
	for i := from; i >= 0; i-- {
		if sa := scopes[i].localsSaveArea; len(sa) > 0 {
			c.execCtx.localsSaveAreaPtr = uintptr(unsafe.Pointer(&sa[0]))
			return
		}
	}
	c.execCtx.localsSaveAreaPtr = 0
}

// acquireLocalsSaveArea returns a locals-mirror buffer for a fresh
// (non-nested) try_table-with-catch scope, sized to hold numLocals*2
// uint64s (16 bytes/local, matching storeLocalToSaveArea's/
// reloadLocalsFromSaveArea's stride). Unlike P1, this never allocates on
// the common path: it reuses callEngine.ehLocalsPool, keyed by
// ehLocalsPoolDepth (the count of currently-open non-nested scopes), so a
// hot loop that repeatedly enters/leaves a try_table at the same nesting
// depth -- even across separate calls to this callEngine -- allocates at
// most once per depth (the first time that depth needs a bigger buffer
// than it currently has). Any stale contents left over from a previous use
// are harmless: the caller (ExitCodeTryTableEnter) is only ever reached
// right before the compiled body unconditionally re-initializes every
// local's slot (storeAllLocalsToSaveArea), so nothing reads the buffer
// before it is fully overwritten.
func (c *callEngine) acquireLocalsSaveArea(numLocals int) []uint64 {
	need := numLocals * 2
	d := c.ehLocalsPoolDepth
	c.ehLocalsPoolDepth++
	if d < len(c.ehLocalsPool) {
		if cap(c.ehLocalsPool[d]) >= need {
			return c.ehLocalsPool[d][:need]
		}
		buf := make([]uint64, need)
		c.ehLocalsPool[d] = buf
		return buf
	}
	buf := make([]uint64, need)
	c.ehLocalsPool = append(c.ehLocalsPool, buf)
	return buf
}

// popCatchScopes truncates scopes to newLen, decrementing ehLocalsPoolDepth
// once for each discarded scope that owned a pooled locals buffer (i.e.
// every scope with a non-nil localsSaveArea -- see acquireLocalsSaveArea),
// keeping the pool's depth bookkeeping in lockstep with activeCatchScopes'
// own stack discipline no matter which of the three call sites
// (ExitCodeTryTableLeave, or handleThrow's per-frame no-match/match
// discards) is doing the popping.
func (c *callEngine) popCatchScopes(scopes []activeCatchScope, newLen int) []activeCatchScope {
	for i := newLen; i < len(scopes); i++ {
		if len(scopes[i].localsSaveArea) > 0 {
			c.ehLocalsPoolDepth--
		}
	}
	return scopes[:newLen]
}

func (c *callEngine) callerModuleInstance() *wasm.ModuleInstance {
	return moduleInstanceFromOpaquePtr(c.execCtx.callerModuleContextPtr)
}

const callStackCeiling = uintptr(50000000) // in uint64 (8 bytes) == 400000000 bytes in total == 400mb.

func (c *callEngine) growStackWithGuarded() (newSP uintptr, newFP uintptr, err error) {
	if nativeapi.StackGuardCheckEnabled {
		nativeapi.CheckStackGuardPage(c.stack)
	}
	newSP, newFP, err = c.growStack()
	if err != nil {
		return
	}
	if nativeapi.StackGuardCheckEnabled {
		c.execCtx.stackBottomPtr = &c.stack[nativeapi.StackGuardCheckGuardPageSize]
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
	newSP, newFP, c.stackTop, c.stack = c.cloneStack(newLen)
	c.execCtx.stackBottomPtr = &c.stack[0]
	return
}

func (c *callEngine) cloneStack(l uintptr) (newSP, newFP, newTop uintptr, newStack []byte) {
	newStack = make([]byte, l)

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

func (c *callEngine) stackIterator(onHostCall bool) experimental.StackIterator {
	c.stackIteratorImpl.reset(c, onHostCall)
	return &c.stackIteratorImpl
}

// stackIterator implements experimental.StackIterator.
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

// Next implements the same method as documented on experimental.StackIterator.
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

// ProgramCounter implements the same method as documented on experimental.StackIterator.
func (si *stackIterator) ProgramCounter() experimental.ProgramCounter {
	return experimental.ProgramCounter(si.pc)
}

// Function implements the same method as documented on experimental.StackIterator.
func (si *stackIterator) Function() experimental.InternalFunction {
	return si
}

// Definition implements the same method as documented on experimental.InternalFunction.
func (si *stackIterator) Definition() api.FunctionDefinition {
	return si.currentDef
}

// SourceOffsetForPC implements the same method as documented on experimental.InternalFunction.
func (si *stackIterator) SourceOffsetForPC(pc experimental.ProgramCounter) uint64 {
	upc := uintptr(pc)
	cm := si.eng.compiledModuleOfAddr(upc)
	return cm.getSourceOffset(upc)
}

// snapshot implements experimental.Snapshot
type snapshot struct {
	sp, fp, top    uintptr
	returnAddress  *byte
	stack          []byte
	savedRegisters [64][2]uint64
	ret            []uint64
	c              *callEngine
}

// Snapshot implements the same method as documented on experimental.Snapshotter.
func (c *callEngine) Snapshot() experimental.Snapshot {
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

// Restore implements the same method as documented on experimental.Snapshot.
func (s *snapshot) Restore(ret []uint64) {
	s.ret = ret
	panic(s)
}

func (s *snapshot) doRestore() {
	spp := *(**uint64)(unsafe.Pointer(&s.sp))
	view := goCallStackView(spp)
	copy(view, s.ret)

	c := s.c
	c.stack = s.stack
	c.stackTop = s.top
	ec := &c.execCtx
	ec.stackBottomPtr = &c.stack[0]
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
