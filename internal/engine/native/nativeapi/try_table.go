package nativeapi

// CatchClauseInstance is a runtime catch clause with resolved tag index.
type CatchClauseInstance struct {
	Kind     byte   // wasm.CatchKindCatch, etc.
	TagIndex uint32 // module-local tag index
}

// TryTableInfo holds try_table metadata assigned during compilation
// and looked up at runtime by try_table ID.
type TryTableInfo struct {
	CatchClauses []CatchClauseInstance
	NumLocals    int
	// ReuseLocals is true for nested same-function try_tables that share
	// the enclosing try_table's locals save area instead of allocating.
	ReuseLocals bool
	// FloorSize is the number of wasm operand-stack values present *below*
	// this try_table's own block params at the point it is entered (the
	// "operand floor" -- see docs/design/eh-side-table.md sec 4.1, category
	// 4). These are values computed before the try that a catch
	// continuation may still consume; nothing inside the try body can
	// observe or mutate them (they are below its own floor), so a single
	// entry-time snapshot would suffice to reconstruct them -- but doing so
	// generally requires threading them through the catch target's block
	// params (so the SSA builder can merge the normal-path value with a
	// landing-pad reload), which touches every branch site that can target
	// a catch label; that is deferred (see call_engine.go's
	// activeCatchScope.savedRegisters doc comment).
	//
	// NOT currently used to skip the callee-saved-register snapshot/restore
	// (an earlier version of this pass gated it on FloorSize == 0 and
	// reverted that after it crashed a real C++/Emscripten-compiled module,
	// see activeCatchScope's doc comment for the full story): FloorSize
	// only accounts for *wasm-level* operand-stack values, but handler
	// blocks unconditionally call reloadAfterCall(), which reads through
	// moduleCtxPtrValue -- a per-function SSA value live across the entire
	// function body regardless of the wasm operand stack, and just as
	// vulnerable to living in a callee-saved register across the enter
	// call as any category-4 value. So FloorSize is computed and
	// serialized but currently inert; a future pass (Opus) should either
	// give moduleCtxPtrValue (and anything else with a whole-function live
	// range) its own memory-based reload in handler blocks -- symmetric to
	// how locals are reloaded -- or otherwise account for it, before this
	// field can safely gate anything.
	FloorSize int
}
