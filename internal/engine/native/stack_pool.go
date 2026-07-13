package native

// This file implements a pool of wasm-execution stacks (the Go-allocated
// []byte buffers callEngine.stack points at -- see call_engine.go), shared
// across every moduleEngine/callEngine created from a single *engine (i.e.
// per-Runtime, not per-module or per-goroutine).
//
// Why this exists: ModuleInstance.ExportedFunction always calls
// Engine.NewFunction fresh (it is never cached), and moduleEngine.NewFunction
// builds a brand new *callEngine for every call in that pattern. Before this
// pool existed, (*callEngine).init unconditionally did
// `c.stack = make([]byte, requiredInitialStackSize())` -- i.e. a fresh,
// zeroed ~10KB allocation on literally every mod.ExportedFunction(name).
// Call(ctx), the single most common call shape. This pool lets that buffer
// be recycled across calls (and across callEngines/modules/goroutines)
// instead.
//
// Bucketing: buffers are kept in stackPoolNumClasses size classes. Class c
// holds buffers whose canonical size is stackPoolBaseSize<<c bytes (10240,
// 20480, 40960, ... -- a doubling sequence anchored at
// requiredInitialStackSize's own default, so the overwhelming common case --
// no oversized param/result signature -- costs nothing extra in either size
// or acquisition logic). A stack that grows (see (*callEngine).growStack) or
// gets replaced by an experimental Snapshot restore (see doRestore) ends up
// some other, not-necessarily-a-clean-multiple-of-the-base length;
// stackPoolClassForRelease floors it into the largest class it still
// satisfies rather than rounding up, so a bucket can never hand out a buffer
// smaller than its class promises. See that function's doc comment for why
// it must NOT be the same function as stackPoolClassForAcquire's ceiling.
const (
	// stackPoolBaseSize is class 0's canonical size. It matches
	// (*callEngine).requiredInitialStackSize's own default -- the size
	// needed by the overwhelming majority of calls (no oversized
	// param/result slice) -- so the default case is unaffected in size,
	// only in allocation source.
	stackPoolBaseSize = 10240

	// stackPoolNumClasses caps the doubling sequence at
	// stackPoolBaseSize<<(stackPoolNumClasses-1) (~5GiB), comfortably past
	// callStackCeiling (~50MB): every stack size this engine can ever
	// legitimately produce has a class. A request or release outside that
	// range (which would require an absurdly large param/result
	// signature, or a stack past the overflow ceiling that growStack
	// already rejects) simply isn't pooled -- see
	// stackPoolClassForAcquire/Release.
	stackPoolNumClasses = 20
)

// stackPoolClassForAcquire returns the smallest class c (0-based) whose
// canonical size (stackPoolBaseSize<<c) is >= n: the bucket to look in (and,
// on a pool miss, the canonical size to allocate) to satisfy a request for n
// bytes. Returns -1 if n exceeds every class's canonical size, meaning:
// don't bother pooling this, just allocate exactly n bytes directly.
func stackPoolClassForAcquire(n int) int {
	c := 0
	size := stackPoolBaseSize
	for size < n {
		if c == stackPoolNumClasses-1 {
			return -1
		}
		size <<= 1
		c++
	}
	return c
}

// stackPoolClassForRelease returns the largest class c (0-based) whose
// canonical size (stackPoolBaseSize<<c) is <= n: the largest bucket a buffer
// of length n is big enough to satisfy. Returns -1 if n is smaller than
// stackPoolBaseSize (shouldn't happen -- every stack this engine ever hands
// out via acquireStack is at least that big) or n is so large it doesn't
// even fit stackPoolNumClasses-1's own floor from below in a useful way
// (practically unreachable; see stackPoolNumClasses).
//
// This is deliberately the floor of n, not stackPoolClassForAcquire's
// ceiling: a buffer's real length n (e.g. a stack grown by
// growStack/cloneStack, or replaced by an experimental Snapshot restore --
// see doRestore -- with some arbitrary, not-necessarily-a-multiple-of-the-
// base size) must only ever be filed under a class whose promised canonical
// size it actually meets or exceeds. Using the ceiling here instead would
// occasionally file an undersized buffer (say, one byte over class c-1's
// size) under class c, where a later acquire for class c would receive it
// believing it has class c's full canonical size -- silent buffer
// under-allocation, a real correctness bug. Flooring instead only ever
// *under-utilizes* a buffer's true capacity (by up to ~2x) or drops it from
// pooling entirely (see stackPoolNumClasses) -- a memory/reuse-efficiency
// tradeoff, never a correctness one.
func stackPoolClassForRelease(n int) int {
	if n < stackPoolBaseSize {
		return -1
	}
	c := 0
	size := stackPoolBaseSize
	for size<<1 <= n {
		if c == stackPoolNumClasses-1 {
			break
		}
		size <<= 1
		c++
	}
	return c
}

// acquireStack returns a wasm-execution stack buffer of at least n bytes,
// reused from e.stackPools when available. The returned boxed pointer is the
// *[]byte wrapper the buffer came out of the pool in (nil on a pool miss);
// pass it back to releaseStack alongside whatever buffer ends up as
// callEngine.stack by the time the call returns, so the same small wrapper
// object can be recycled indefinitely instead of allocated fresh once the
// pool has warmed up -- boxing a []byte value (as opposed to a pointer to
// one) directly into sync.Pool's interface{} parameter would itself
// allocate on every single release (a slice header doesn't fit in the
// interface's single data word), defeating a good chunk of the point.
func (e *engine) acquireStack(n int) (buf []byte, boxed *[]byte) {
	class := stackPoolClassForAcquire(n)
	if class < 0 {
		return make([]byte, n), nil
	}
	if v := e.stackPools[class].Get(); v != nil {
		boxed = v.(*[]byte)
		return *boxed, boxed
	}
	return make([]byte, stackPoolBaseSize<<uint(class)), nil
}

// releaseStack returns stack -- callEngine.stack as of the outermost call's
// return, which may not be the same buffer acquireStack originally handed
// out (see growStack/cloneStack and the experimental Snapshotter's
// doRestore, either of which can replace callEngine.stack mid-call) -- to
// e.stackPools, bucketed by its own current length via
// stackPoolClassForRelease. boxed should be whatever acquireStack returned
// earlier in the same top-level call (nil if that call missed the pool);
// see acquireStack's doc comment for why reusing it (rather than allocating
// a new *[]byte here) matters.
func (e *engine) releaseStack(stack []byte, boxed *[]byte) {
	class := stackPoolClassForRelease(len(stack))
	if class < 0 {
		return
	}
	if boxed == nil {
		boxed = new([]byte)
	}
	*boxed = stack
	e.stackPools[class].Put(boxed)
}
