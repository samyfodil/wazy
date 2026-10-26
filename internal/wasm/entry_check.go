package wasm

import (
	"cmp"
	"slices"
)

// PreemptBudget is how much wasm code, in bytes, compiled code may run between
// two preemption checks: about as long as it makes a stop-the-world wait. Loops
// are checked against it at their headers (see frontend.Compiler.emitPreemptCheck),
// and calls at the entry of the functions FunctionNeedsEntryCheck names.
const PreemptBudget = 1 << 16

// entryCheckBuilder works out which functions need a preemption check on entry,
// while validation walks their bodies.
//
// Loops check at their headers, which bounds everything but code that repeats
// without one: recursion, direct or through other functions or tail calls, and a
// loop-free call graph that does exponentially much work at a fixed depth (f calls
// g twice, g calls h twice, ...). Every cycle of the call graph gets a function
// checked on entry: on a cycle through the hub below, the one that calls through
// it, and on any other, every function on it. Of the rest, worked out callee
// first, a function's unchecked work is its own body plus the unchecked work of
// each call it makes; when its calls' share passes PreemptBudget, its heaviest
// callees are checked on entry until it does not. Its own body runs once a call,
// however large, and checking callees cannot shorten it, so it is not held to the
// budget: a callee that is called from hot code elsewhere would pay for it on
// every call.
//
// An indirect call (call_indirect, call_ref and their tail-call forms) may reach
// any function whose reference is taken, so it counts as the heaviest of them
// that is not checked, and checking a callee here means checking that one. A call
// that may run another module's code is checked where it is made instead (see
// frontend.Compiler.checkFirst) and counts as nothing here: a call to an
// import, and an indirect call in a module that references to other modules'
// functions can reach (see Module.foreignFuncRefs). Once in, the other module's
// own checks bound the call, but only the check before it bounds how many such
// calls run: a chain of modules, each calling the one before it twice, is a call
// tree no one module sees. A cycle through other modules passes one of those
// checks too.
//
// It also works out which calls are sure to be short, which lets a loop making
// them keep the cheaper checks of a loop that calls nothing (see
// frontend.Compiler.tunePreemptCheck): a call to a function with no loop, no bulk
// memory or table operation, no indirect call and no call to an import, which
// calls only functions like it and none recursively, runs at most its own body
// plus each of its calls' work.
type entryCheckBuilder struct {
	// imports is the number of imported functions, which head the index space.
	imports uint32
	// size is each local function's body size in bytes.
	size []uint32
	// edges holds caller/callee pairs: the local index of the caller and the
	// function index of the callee, one per direct call or return_call.
	edges []Index
	// indirect counts each local function's indirect calls, unless foreign.
	indirect []uint32
	// foreign is Module.ForeignFuncRefs: indirect calls are checked where they are
	// made, like calls to imports.
	foreign bool
	// unbounded marks the local functions one call of which may run for as long
	// as it likes without recursing: see markUnbounded.
	unbounded []bool
}

// newEntryCheckBuilder returns a builder for m, whose function index space starts
// with imports imported functions.
func newEntryCheckBuilder(m *Module, imports uint32) *entryCheckBuilder {
	b := &entryCheckBuilder{
		imports:   imports,
		size:      make([]uint32, len(m.FunctionSection)),
		indirect:  make([]uint32, len(m.FunctionSection)),
		unbounded: make([]bool, len(m.FunctionSection)),
		foreign:   m.ForeignFuncRefs,
	}
	for i := range b.size {
		b.size[i] = uint32(len(m.CodeSection[i].Body))
	}
	return b
}

// call records that the local function idx calls the function callee. It is a
// no-op on a nil builder.
func (b *entryCheckBuilder) call(idx, callee Index) {
	switch {
	case b == nil:
	case callee >= b.imports:
		b.edges = append(b.edges, idx, callee-b.imports)
	default:
		b.unbounded[idx] = true
	}
}

// callIndirect records an indirect call made by the local function idx. It is a
// no-op on a nil builder.
func (b *entryCheckBuilder) callIndirect(idx Index) {
	switch {
	case b == nil:
	case b.foreign:
		b.unbounded[idx] = true
	default:
		b.indirect[idx]++
	}
}

// markUnbounded records that the local function idx has a loop or a bulk memory
// or table operation. It is a no-op on a nil builder.
func (b *entryCheckBuilder) markUnbounded(idx Index) {
	if b != nil {
		b.unbounded[idx] = true
	}
}

// solve returns, per local function, whether it needs a preemption check on
// entry, and how much wasm code a call to it runs at most: zero unless that is
// sure to be short (see entryCheckBuilder) and within PreemptBudget. referenced
// holds the function indices whose references may be taken.
func (b *entryCheckBuilder) solve(referenced map[Index]struct{}) (needs []bool, work []uint32) {
	if b.foreign {
		referenced = nil // no indirect call reaches them unchecked
	}
	n := len(b.size)
	// Node n stands for "any function an indirect call can reach".
	hub := n
	succStart := make([]uint32, n+2)
	for i := 0; i < len(b.edges); i += 2 {
		succStart[b.edges[i]+1]++
	}
	for f := 0; f < n; f++ {
		if b.indirect[f] > 0 {
			succStart[f+1]++
		}
	}
	for idx := range referenced {
		if idx >= b.imports && int(idx-b.imports) < n {
			succStart[hub+1]++
		}
	}
	for i := 0; i <= n; i++ {
		succStart[i+1] += succStart[i]
	}
	succ := make([]int32, succStart[n+1])
	fill := append([]uint32(nil), succStart[:n+1]...)
	for i := 0; i < len(b.edges); i += 2 {
		f := b.edges[i]
		succ[fill[f]] = int32(b.edges[i+1])
		fill[f]++
	}
	for f := 0; f < n; f++ {
		if b.indirect[f] > 0 {
			succ[fill[f]] = int32(hub)
			fill[f]++
		}
	}
	for idx := range referenced {
		if idx >= b.imports && int(idx-b.imports) < n {
			succ[fill[hub]] = int32(idx - b.imports)
			fill[hub]++
		}
	}

	needs = make([]bool, n+1)
	work = make([]uint32, n)
	sccs := tarjan(n+1, succStart, succ)
	// uc is each node's unchecked work: what running it costs up to the next
	// check, counting a callee that is checked on entry as nothing.
	uc := make([]uint64, n+1)
	contribution := func(c int32) uint64 {
		if needs[c] {
			return 0
		}
		return uc[c]
	}
	// byWork holds what an indirect call can reach and is not checked, heaviest
	// first, once that is known. Its head is the hub's work.
	var byWork []int32
	check := func(c int32) {
		if c == int32(hub) {
			c = byWork[0]
		}
		needs[c] = true
		for len(byWork) > 0 && needs[byWork[0]] {
			byWork = byWork[1:]
		}
		uc[hub] = 0
		if len(byWork) > 0 {
			uc[hub] = uc[byWork[0]]
		}
	}
	for _, scc := range sccs { // callees before callers
		switch {
		case len(scc) > 1 && slices.Contains(scc, int32(hub)):
			scc = b.breakHubCycles(scc, succStart, succ, needs)
		case len(scc) > 1:
			for _, v := range scc {
				needs[v] = true
			}
		default:
			if v := scc[0]; slices.Contains(succ[succStart[v]:succStart[v+1]], v) {
				needs[v] = true
			}
		}
		for _, v := range scc {
			if v == int32(hub) {
				for _, c := range succ[succStart[v]:succStart[v+1]] {
					if contribution(c) > 0 {
						byWork = append(byWork, c)
					}
				}
				slices.SortFunc(byWork, func(x, y int32) int {
					return cmp.Or(cmp.Compare(uc[y], uc[x]), cmp.Compare(x, y))
				})
				if len(byWork) > 0 {
					uc[v] = uc[byWork[0]]
				}
				continue
			}
			b.bound(v, succStart, succ, uc, contribution, check)
		}
		if v := scc[0]; len(scc) == 1 && v < int32(hub) && !b.unbounded[v] && b.indirect[v] == 0 {
			// A call to v itself finds work[v] still zero.
			w := uint64(b.size[v])
			for _, c := range succ[succStart[v]:succStart[v+1]] {
				if work[c] == 0 {
					w = PreemptBudget + 1
					break
				}
				w += uint64(work[c])
			}
			if w <= PreemptBudget {
				work[v] = uint32(w)
			}
		}
	}
	return needs[:n], work
}

// breakHubCycles checks on entry the functions of scc, a strongly connected
// component holding the hub, that make an indirect call, since every cycle
// through the hub passes through one of them. Of the rest, which call only
// directly, it checks those still on a cycle. It returns scc in the order to work
// out unchecked work in: the rest callees first, then the hub, then the functions
// that call through it.
func (b *entryCheckBuilder) breakHubCycles(scc []int32, succStart []uint32, succ []int32, needs []bool) []int32 {
	hub := int32(len(b.size))
	pos := make([]int32, hub) // a function's index in rest, plus one
	var rest, callers []int32
	for _, v := range scc {
		switch {
		case v == hub:
		case b.indirect[v] > 0:
			needs[v] = true
			callers = append(callers, v)
		default:
			rest = append(rest, v)
			pos[v] = int32(len(rest))
		}
	}
	start := make([]uint32, len(rest)+1)
	var calls []int32
	for i, v := range rest {
		for _, c := range succ[succStart[v]:succStart[v+1]] {
			if c != hub && pos[c] > 0 {
				calls = append(calls, pos[c]-1)
			}
		}
		start[i+1] = uint32(len(calls))
	}
	order := make([]int32, 0, len(scc))
	for _, sub := range tarjan(len(rest), start, calls) {
		for _, i := range sub {
			if len(sub) > 1 || slices.Contains(calls[start[i]:start[i+1]], i) {
				needs[rest[i]] = true
			}
			order = append(order, rest[i])
		}
	}
	return append(append(order, hub), callers...)
}

// bound computes the unchecked work of the local function v, checking its
// heaviest callees on entry until the work of its calls is within PreemptBudget.
func (b *entryCheckBuilder) bound(v int32, succStart []uint32, succ []int32, uc []uint64, contribution func(int32) uint64, check func(int32)) {
	hub := int32(len(b.size))
	calls := succ[succStart[v]:succStart[v+1]]
	for {
		var callWork uint64
		heaviest, heaviestWork := int32(-1), uint64(0)
		for _, c := range calls {
			w := contribution(c)
			if c == hub {
				w *= uint64(b.indirect[v])
			}
			callWork += w
			if w > heaviestWork {
				heaviest, heaviestWork = c, w
			}
		}
		uc[v] = uint64(b.size[v]) + callWork
		if callWork <= PreemptBudget {
			return
		}
		check(heaviest)
	}
}

// tarjan returns the strongly connected components of the graph whose node v has
// the successors succ[start[v]:start[v+1]], callees before callers. It keeps its
// own stack rather than recursing, since a call chain can be as deep as a module
// has functions.
func tarjan(n int, start []uint32, succ []int32) [][]int32 {
	const unvisited = -1
	index := make([]int32, n)
	low := make([]int32, n)
	onStack := make([]bool, n)
	for i := range index {
		index[i] = unvisited
	}
	var stack []int32
	var sccs [][]int32
	type frame struct {
		v    int32
		next uint32
	}
	var frames []frame
	next := int32(0)
	for root := 0; root < n; root++ {
		if index[root] != unvisited {
			continue
		}
		frames = append(frames[:0], frame{v: int32(root), next: start[root]})
		index[root], low[root] = next, next
		next++
		stack = append(stack, int32(root))
		onStack[root] = true
		for len(frames) > 0 {
			top := &frames[len(frames)-1]
			v := top.v
			if top.next < start[v+1] {
				w := succ[top.next]
				top.next++
				if index[w] == unvisited {
					index[w], low[w] = next, next
					next++
					stack = append(stack, w)
					onStack[w] = true
					frames = append(frames, frame{v: w, next: start[w]})
				} else if onStack[w] {
					low[v] = min(low[v], index[w])
				}
				continue
			}
			frames = frames[:len(frames)-1]
			if len(frames) > 0 {
				parent := frames[len(frames)-1].v
				low[parent] = min(low[parent], low[v])
			}
			if low[v] == index[v] {
				var scc []int32
				for {
					w := stack[len(stack)-1]
					stack = stack[:len(stack)-1]
					onStack[w] = false
					scc = append(scc, w)
					if w == v {
						break
					}
				}
				sccs = append(sccs, scc)
			}
		}
	}
	return sccs
}

// foreignFuncRefs reports whether an indirect call of m may reach another
// module's function, which entryCheckBuilder cannot see into. A reference to a
// function is made by ref.func, which names a declared function, or comes in from
// outside: through a table another module or the host can reach, or as anything
// an import or export carries. functions, globals and tags hold the type of each
// function, global and tag, imports first. An index out of range, which only a
// module not validated yet can hold, counts as carrying one.
func (m *Module) foreignFuncRefs(declared map[Index]struct{}, functions []Index, globals []GlobalType, tags []Index) bool {
	for idx := range declared {
		if idx < m.ImportFunctionCount {
			return true
		}
	}
	carriesRef := func(typeIndex Index) bool {
		if int(typeIndex) >= len(m.TypeSection) {
			return true
		}
		t := &m.TypeSection[typeIndex]
		return slices.ContainsFunc(t.Params, ValueType.IsRef) || slices.ContainsFunc(t.Results, ValueType.IsRef)
	}
	typeOf := func(types []Index, i Index) Index {
		if int(i) < len(types) {
			return types[i]
		}
		return ^Index(0)
	}
	for i := range m.ImportSection {
		switch imp := &m.ImportSection[i]; imp.Type {
		case ExternTypeTable:
			return true
		case ExternTypeFunc:
			if carriesRef(imp.DescFunc) {
				return true
			}
		case ExternTypeGlobal:
			if imp.DescGlobal.ValType.IsRef() {
				return true
			}
		case ExternTypeTag:
			if carriesRef(imp.DescTag) {
				return true
			}
		}
	}
	for i := range m.ExportSection {
		switch exp := &m.ExportSection[i]; exp.Type {
		case ExternTypeTable:
			return true
		case ExternTypeFunc:
			if carriesRef(typeOf(functions, exp.Index)) {
				return true
			}
		case ExternTypeGlobal:
			if int(exp.Index) >= len(globals) || globals[exp.Index].ValType.IsRef() {
				return true
			}
		case ExternTypeTag:
			if carriesRef(typeOf(tags, exp.Index)) {
				return true
			}
		}
	}
	return false
}
