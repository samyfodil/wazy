package wazevoapi

// EhClause is one compiled catch clause belonging to an EhEntry. Unlike the
// interpreter's exceptionTableCatchClause (which has a single targetPC per
// clause), the compiled analogue needs a per-clause LandingPad because the
// wazevo frontend compiles one dedicated handler block per catch clause
// (each reloading a different set of exception params / exnref and jumping
// to a different wasm target block).
type EhClause struct {
	// Kind is one of wasm.CatchKindCatch, CatchKindCatchRef, CatchKindCatchAll,
	// CatchKindCatchAllRef.
	Kind byte
	// TagIndex is the module-local tag index (meaningful for Catch/CatchRef only).
	TagIndex uint32
	// LandingPad is the executable offset of this clause's handler block.
	// Function-relative as constructed by the compiler; rebased to an
	// absolute address (like sourceMap.executableOffsets) when the
	// compiledModule's executable is finalized / deserialized from cache.
	LandingPad uintptr
}

// EhEntry is one PC-range entry in a function's exception side table: the
// compiled analogue of the interpreter's exceptionTableEntry
// (interpreter/interpreter.go). Entries for a given function are stored in
// *encounter order* -- i.e. a nested try_table's entry is appended after its
// lexically-enclosing try_table's entry, exactly mirroring how the
// interpreter builds its exceptionTable during lowering. A throw searches a
// function's entries **backwards**, so this ordering alone (no separate
// depth field) gives innermost-try-first matching, identical to
// searchExceptionTable's `for i := len(table) - 1; i >= 0; i--` loop.
type EhEntry struct {
	// StartOffset/EndOffset delimit one contiguous run of native code
	// belonging to this try_table's body: any return address in
	// [StartOffset, EndOffset) is considered "inside" this try for the
	// purposes of exception matching. Function-relative as constructed;
	// rebased to absolute addresses at the same time as LandingPad.
	//
	// A single try_table may be represented by *multiple* EhEntry values
	// (one per contiguous run) if block layout did not keep its body's
	// native code contiguous; all such runs share the same Clauses and
	// TryTableID.
	StartOffset, EndOffset uintptr
	// Clauses are this try_table's catch clauses, in declaration order (the
	// same order used for first-match semantics today).
	Clauses []EhClause
	// TryTableID indexes into compiledModule.tryTableInfo, giving access to
	// NumLocals/ReuseLocals so the throw path can correctly restore
	// execCtx.localsSaveAreaPtr when unwinding past an abandoned frame that
	// had its own (now-discarded) locals save area.
	TryTableID int
}

// ThrowFrame is one frame yielded by the throw-time stack walk (the
// exception-handling variant of unwindStack that additionally recovers each
// frame's own base, not just its return address). Populated differently per
// ISA:
//
//   - arm64 (backend/isa/arm64/unwind_stack.go): frames are chained via an
//     in-stack frame_size word, so SP is directly, self-containedly
//     recoverable during the walk; FP is unused (left zero) since arm64 has
//     no dedicated frame-pointer register.
//   - amd64 (backend/isa/amd64/stack.go): frames are chained via the
//     classic RBP chain, so FP (=RBP) is directly recoverable during the
//     walk, but SP is not (amd64 has no on-stack frame_size marker); the
//     caller (call_engine.go) computes SP = FP - FrameSize() for whichever
//     single frame turns out to match, using the compiled function's
//     FrameSize (backend.Machine.FrameSize) recorded at compile time.
type ThrowFrame struct {
	ReturnAddress uintptr
	SP, FP        uintptr
}
