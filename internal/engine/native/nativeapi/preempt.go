package nativeapi

// Go cannot preempt compiled wasm: asynchronous preemption declines a PC that
// is not Go's. A loop that never returns to Go therefore holds up every
// stop-the-world, and with it every goroutine in the process, until it ends.
// Compiled loops read the word the runtime uses to ask a goroutine to stop
// instead, the one a Go function prologue reads, and return to Go when it is
// set. Both facts below are runtime internals; TestPreemptWordLayout pins them
// against the toolchain's own sources.
const (
	// GStackguard0Offset is the offset of stackguard0 in the runtime's g, right
	// after g.stack (two words).
	GStackguard0Offset = 16
	// StackPreempt is what the runtime stores in g.stackguard0 to ask a
	// goroutine to stop: runtime/stack.go's uintptrMask & -1314.
	StackPreempt = uint64(0xfffffffffffffade)
)
