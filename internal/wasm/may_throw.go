package wasm

// mayThrowBuilder works out which functions can throw a wasm exception, while validation walks their bodies.
//
// A function may throw when it contains a throw or throw_ref, makes an indirect call (call_indirect,
// call_ref and their tail-call forms) whose target is unknown, or calls a function that may throw. Every
// imported function may throw, whether it is a host function or another module's. This is a conservative
// whole-module fixpoint: a throw that the function itself always catches still counts.
type mayThrowBuilder struct {
	// direct is indexed by function index and set for a function that throws by itself.
	direct []bool
	// edges holds caller/callee function-index pairs, one per direct call or return_call.
	edges []Index
	// imports is the number of imported functions, which head the index space.
	imports uint32
}

// throws records that the local function idx throws by itself. It is a no-op on a nil builder, which is
// what validation has when exception handling is off.
func (b *mayThrowBuilder) throws(idx Index) {
	if b != nil {
		b.direct[b.imports+idx] = true
	}
}

// call records that the local function idx calls the function callee (an index into the whole space).
func (b *mayThrowBuilder) call(idx, callee Index) {
	if b != nil {
		b.edges = append(b.edges, b.imports+idx, callee)
	}
}

// solve returns, per function index, whether the function may throw.
func (b *mayThrowBuilder) solve() []bool {
	n := len(b.direct)
	may := b.direct
	for i := uint32(0); i < b.imports; i++ {
		may[i] = true
	}
	// Callers of each callee, in compressed rows, so the propagation below is linear in the edges.
	start := make([]uint32, n+1)
	for i := 1; i < len(b.edges); i += 2 {
		start[b.edges[i]+1]++
	}
	for i := 0; i < n; i++ {
		start[i+1] += start[i]
	}
	callers := make([]Index, len(b.edges)/2)
	fill := append([]uint32(nil), start[:n]...)
	for i := 0; i < len(b.edges); i += 2 {
		callee := b.edges[i+1]
		callers[fill[callee]] = b.edges[i]
		fill[callee]++
	}
	work := make([]Index, 0, n)
	for i := range may {
		if may[i] {
			work = append(work, Index(i))
		}
	}
	for len(work) > 0 {
		f := work[len(work)-1]
		work = work[:len(work)-1]
		for _, caller := range callers[start[f]:start[f+1]] {
			if !may[caller] {
				may[caller] = true
				work = append(work, caller)
			}
		}
	}
	return may
}
