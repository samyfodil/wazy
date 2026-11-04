package wasm

// CallEffects are the ways a call can change what its caller has cached about the instance.
type CallEffects uint8

const (
	// CallEffectGrowMemory is set when the call may grow a memory, which moves its base and length.
	CallEffectGrowMemory CallEffects = 1 << iota
	// CallEffectSetGlobal is set when the call may set a mutable global.
	CallEffectSetGlobal

	callEffectsAll = CallEffectGrowMemory | CallEffectSetGlobal
)

// callEffectsBuilder works out FunctionCallEffects while validation walks the function bodies. A call to
// an import, an indirect call and a function implemented in Go may do anything, since they can run code
// this module does not see; every other function has the effects of its own body and of its callees.
type callEffectsBuilder struct {
	// direct is indexed by local function index and holds the effects of the function's own body.
	direct []CallEffects
	// edges holds caller/callee local function-index pairs, one per direct call or return_call.
	edges []Index
	// imports is the number of imported functions, which head the index space.
	imports uint32
}

func newCallEffectsBuilder(m *Module, imports uint32) *callEffectsBuilder {
	return &callEffectsBuilder{direct: make([]CallEffects, len(m.FunctionSection)), imports: imports}
}

// mark records that the local function idx has effects e by itself. It is a no-op on a nil builder.
func (b *callEffectsBuilder) mark(idx Index, e CallEffects) {
	if b != nil {
		b.direct[idx] |= e
	}
}

// call records that the local function idx calls the function callee (an index into the whole space).
// It is a no-op on a nil builder.
func (b *callEffectsBuilder) call(idx, callee Index) {
	switch {
	case b == nil:
	case callee >= b.imports:
		b.edges = append(b.edges, idx, callee-b.imports)
	default:
		b.direct[idx] = callEffectsAll
	}
}

// solve returns, per local function, the effects a call to it may have.
func (b *callEffectsBuilder) solve() []CallEffects {
	n := len(b.direct)
	effects := b.direct
	// Callers of each callee, in compressed rows, so the propagation below is linear in the edges times
	// the number of effects.
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
	for i, e := range effects {
		if e != 0 {
			work = append(work, Index(i))
		}
	}
	for len(work) > 0 {
		f := work[len(work)-1]
		work = work[:len(work)-1]
		for _, caller := range callers[start[f]:start[f+1]] {
			if effects[caller]|effects[f] != effects[caller] {
				effects[caller] |= effects[f]
				work = append(work, caller)
			}
		}
	}
	return effects
}
