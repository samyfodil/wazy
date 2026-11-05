package frontend

import (
	"cmp"
	"slices"
	"strconv"

	"github.com/samyfodil/wazy/internal/engine/native/ssa"
	"github.com/samyfodil/wazy/internal/leb128"
	"github.com/samyfodil/wazy/internal/wasm"
)

// A vector sum is a straight-line run of wasm that leaves one i32 on the stack, a sum some of whose
// terms come in fours: the same computation on four consecutive i32 elements of the same rows, such
// as a block's sum of absolute differences unrolled along a row. Each four is computed on 128-bit
// vectors and summed across its lanes.
//
// The run is lowered twice. Where every row the vectors read is in bounds, which one check per row
// tells, the vector code runs; otherwise the run's own lowering does, trapping where it would. Both
// leave the locals the run sets as it would.

// VecSumHook, when set, is told of each vector sum the frontend lowers.
var VecSumHook func(fn wasm.Index, pc int)

// vecSumMaxSpan bounds the bytes one row of a vector sum covers.
const vecSumMaxSpan = 4096

// vecSumMaxOps bounds how far one scan for vector sums simulates the wasm.
const vecSumMaxOps = 4096

const (
	vsEntry = iota + 1 // the value of local k where the run starts
	vsConst            // the i32 k
	vsLoad             // the i32 at a, plus the memarg offset k
	vsBin              // op applied to a and b
)

// vsNode is a value the run computes. Equal nodes have equal indices.
type vsNode struct {
	kind uint8
	op   wasm.Opcode
	a, b int32
	k    uint64
}

// vsSim simulates a run of wasm on symbolic values.
type vsSim struct {
	nodes  []vsNode
	ids    map[vsNode]int32
	locals map[uint32]int32
	stack  []vsSlot
	loads  []int32
}

// vsSlot is a value on the stack, computed by the code from start on.
type vsSlot struct {
	id    int32
	start int
}

func newVsSim() *vsSim {
	return &vsSim{ids: map[vsNode]int32{}, locals: map[uint32]int32{}}
}

func (s *vsSim) intern(n vsNode) int32 {
	if id, ok := s.ids[n]; ok {
		return id
	}
	id := int32(len(s.nodes))
	s.nodes = append(s.nodes, n)
	s.ids[n] = id
	if n.kind == vsLoad {
		s.loads = append(s.loads, id)
	}
	return id
}

func (s *vsSim) local(k uint32) int32 {
	if id, ok := s.locals[k]; ok {
		return id
	}
	id := s.intern(vsNode{kind: vsEntry, k: uint64(k)})
	s.locals[k] = id
	return id
}

// step simulates the instruction at pc, and returns where the next one is, or false where it is one the
// simulation does not take.
func (s *vsSim) step(body []byte, pc int) (int, bool) {
	op := body[pc]
	switch op {
	case wasm.OpcodeLocalGet, wasm.OpcodeLocalSet, wasm.OpcodeLocalTee:
		k, n, err := leb128.LoadUint32(body[pc+1:])
		if err != nil {
			return 0, false
		}
		switch op {
		case wasm.OpcodeLocalGet:
			s.stack = append(s.stack, vsSlot{id: s.local(k), start: pc})
		case wasm.OpcodeLocalSet:
			if len(s.stack) == 0 {
				return 0, false
			}
			s.locals[k] = s.stack[len(s.stack)-1].id
			s.stack = s.stack[:len(s.stack)-1]
		default:
			if len(s.stack) == 0 {
				return 0, false
			}
			s.locals[k] = s.stack[len(s.stack)-1].id
		}
		return pc + 1 + int(n), true
	case wasm.OpcodeI32Const:
		v, n, err := leb128.LoadInt32(body[pc+1:])
		if err != nil {
			return 0, false
		}
		s.stack = append(s.stack, vsSlot{id: s.intern(vsNode{kind: vsConst, k: uint64(uint32(v))}), start: pc})
		return pc + 1 + int(n), true
	case wasm.OpcodeI32Load:
		align, n, err := leb128.LoadUint32(body[pc+1:])
		if err != nil || align&memArgMultiMemoryFlag != 0 || len(s.stack) == 0 {
			return 0, false
		}
		off, m, err := leb128.LoadUint64(body[pc+1+int(n):])
		if err != nil {
			return 0, false
		}
		top := &s.stack[len(s.stack)-1]
		top.id = s.intern(vsNode{kind: vsLoad, a: top.id, k: off})
		return pc + 1 + int(n) + int(m), true
	case wasm.OpcodeI32Add, wasm.OpcodeI32Sub, wasm.OpcodeI32Mul, wasm.OpcodeI32And, wasm.OpcodeI32Or,
		wasm.OpcodeI32Xor, wasm.OpcodeI32Shl, wasm.OpcodeI32ShrS, wasm.OpcodeI32ShrU:
		if len(s.stack) < 2 {
			return 0, false
		}
		x, y := s.stack[len(s.stack)-2], s.stack[len(s.stack)-1]
		s.stack = s.stack[:len(s.stack)-1]
		s.stack[len(s.stack)-1].id = s.intern(vsNode{kind: vsBin, op: op, a: x.id, b: y.id})
		return pc + 1, true
	case wasm.OpcodeDrop:
		if len(s.stack) == 0 {
			return 0, false
		}
		s.stack = s.stack[:len(s.stack)-1]
		return pc + 1, true
	case wasm.OpcodeNop:
		return pc + 1, true
	}
	return 0, false
}

// vsPlan is how a vector sum computes its total.
type vsPlan struct {
	groups  [][4]int32 // each four terms, in the order of their elements
	rest    []int32    // the other terms
	streams []vsStream
}

// vsStream is a row the vectors read: end bytes from the address base plus lo. A load of base plus c,
// with the memarg offset off, reads at c-lo+off into it.
type vsStream struct {
	base   int32
	lo, hi int32 // the least and the greatest c
	end    uint64
	addr   ssa.Value // the row's start in host memory
}

// at returns where in st the load of base plus c with offset off reads.
func (st *vsStream) at(c int32, off uint64) uint64 { return uint64(int64(c)-int64(st.lo)) + off }

// vsAddr splits an address into a value and the i32 constants added to it.
func (s *vsSim) vsAddr(id int32) (base, c int32) {
	for {
		n := s.nodes[id]
		if n.kind != vsBin || n.op != wasm.OpcodeI32Add {
			return id, c
		}
		switch {
		case s.nodes[n.b].kind == vsConst:
			c += int32(s.nodes[n.b].k)
			id = n.a
		case s.nodes[n.a].kind == vsConst:
			c += int32(s.nodes[n.a].k)
			id = n.b
		default:
			return id, c
		}
	}
}

// abs reports whether id is (d ^ s) - s with s = d >> 31, arithmetic: |d|, wrapping.
func (s *vsSim) abs(id int32) (d int32, ok bool) {
	n := s.nodes[id]
	if n.kind != vsBin || n.op != wasm.OpcodeI32Sub {
		return 0, false
	}
	x, sign := s.nodes[n.a], s.nodes[n.b]
	if x.kind != vsBin || x.op != wasm.OpcodeI32Xor || sign.kind != vsBin || sign.op != wasm.OpcodeI32ShrS {
		return 0, false
	}
	if k := s.nodes[sign.b]; k.kind != vsConst || k.k&31 != 31 {
		return 0, false
	}
	d = sign.a
	if !(x.a == d && x.b == n.b || x.b == d && x.a == n.b) {
		return 0, false
	}
	return d, true
}

// loadsUnder returns the loads id computes from.
func (s *vsSim) loadsUnder(id int32) []int32 {
	var out []int32
	seen := map[int32]bool{}
	var walk func(id int32)
	walk = func(id int32) {
		if seen[id] {
			return
		}
		seen[id] = true
		switch n := s.nodes[id]; n.kind {
		case vsLoad:
			out = append(out, id)
		case vsBin:
			walk(n.a)
			walk(n.b)
		}
	}
	walk(id)
	return out
}

// hasLoad reports whether id reads memory.
func (s *vsSim) hasLoad(id int32) bool {
	n := s.nodes[id]
	switch n.kind {
	case vsLoad:
		return true
	case vsBin:
		return s.hasLoad(n.a) || s.hasLoad(n.b)
	}
	return false
}

// plan returns how to compute the sum root as a vector sum, or nil. loads are all the loads the run
// makes, each of which has to be in a row it checks.
func (s *vsSim) plan(root int32, loads []int32) *vsPlan {
	var terms []int32
	var flatten func(id int32)
	flatten = func(id int32) {
		if n := s.nodes[id]; n.kind == vsBin && n.op == wasm.OpcodeI32Add {
			flatten(n.a)
			flatten(n.b)
			return
		}
		terms = append(terms, id)
	}
	flatten(root)
	if len(terms) < 4 {
		return nil
	}

	// A term's shape is its computation with the constants added to its load addresses left out: terms
	// of one shape differ only in where they load. Its loads are in the order the shape lists them.
	shapes := map[int32]string{}
	var shape func(id int32) string
	shape = func(id int32) string {
		if sh, ok := shapes[id]; ok {
			return sh
		}
		n := s.nodes[id]
		var sh string
		switch n.kind {
		case vsLoad:
			base, _ := s.vsAddr(n.a)
			sh = "L" + strconv.Itoa(int(base))
		case vsBin:
			sh = "(" + strconv.Itoa(int(n.op)) + " " + shape(n.a) + " " + shape(n.b) + ")"
		default:
			sh = "#" + strconv.Itoa(int(id))
		}
		shapes[id] = sh
		return sh
	}
	// A load's position is c plus its offset: where it reads, but for wrapping, which the row's check
	// rules out.
	loadsOf := func(id int32) []int64 {
		var out []int64
		var walk func(id int32)
		walk = func(id int32) {
			switch n := s.nodes[id]; n.kind {
			case vsLoad:
				_, c := s.vsAddr(n.a)
				out = append(out, int64(c)+int64(n.k))
			case vsBin:
				walk(n.a)
				walk(n.b)
			}
		}
		walk(id)
		return out
	}

	p := &vsPlan{}
	byShape := map[string][]int32{}
	var order []string
	for _, t := range terms {
		if !s.hasLoad(t) {
			p.rest = append(p.rest, t)
			continue
		}
		sh := shape(t)
		if _, ok := byShape[sh]; !ok {
			order = append(order, sh)
		}
		byShape[sh] = append(byShape[sh], t)
	}
	laneLoads := map[int32]bool{}
	for _, sh := range order {
		ts := byShape[sh]
		cs := make(map[int32][]int64, len(ts))
		for _, t := range ts {
			cs[t] = loadsOf(t)
		}
		slices.SortStableFunc(ts, func(x, y int32) int { return cmp.Compare(cs[x][0], cs[y][0]) })
		next := func(x, y int32) bool { // y loads each element 4 bytes past where x does
			for i := range cs[x] {
				if cs[y][i] != cs[x][i]+4 {
					return false
				}
			}
			return true
		}
		for i := 0; i < len(ts); {
			if i+3 < len(ts) && next(ts[i], ts[i+1]) && next(ts[i+1], ts[i+2]) && next(ts[i+2], ts[i+3]) {
				g := [4]int32{ts[i], ts[i+1], ts[i+2], ts[i+3]}
				if !s.lanesOK(g, laneLoads) {
					return nil
				}
				p.groups = append(p.groups, g)
				i += 4
				continue
			}
			return nil // A term that loads and is in no four: nothing would check its bounds.
		}
	}
	if len(p.groups) == 0 {
		return nil
	}
	for _, l := range loads {
		if !laneLoads[l] {
			return nil
		}
	}

	streamOf := map[int32]int{}
	for _, l := range loads {
		n := s.nodes[l]
		base, c := s.vsAddr(n.a)
		if s.hasLoad(base) {
			return nil
		}
		k, ok := streamOf[base]
		if !ok {
			k = len(p.streams)
			streamOf[base] = k
			p.streams = append(p.streams, vsStream{base: base, lo: c, hi: c})
		}
		st := &p.streams[k]
		st.lo, st.hi = min(st.lo, c), max(st.hi, c)
		if int64(st.hi)-int64(st.lo) > vecSumMaxSpan {
			return nil
		}
	}
	for _, l := range loads {
		n := s.nodes[l]
		base, c := s.vsAddr(n.a)
		st := &p.streams[streamOf[base]]
		st.end = max(st.end, st.at(c, n.k)+4)
		if st.end > 1<<32-1 {
			return nil
		}
	}
	return p
}

// lanesOK reports whether the four terms l compute lane by lane as vector operations, and marks their
// loads in lane.
func (s *vsSim) lanesOK(l [4]int32, lane map[int32]bool) bool {
	if l[0] == l[1] && l[0] == l[2] && l[0] == l[3] {
		return !s.hasLoad(l[0])
	}
	var ns [4]vsNode
	for i, id := range l {
		ns[i] = s.nodes[id]
		if ns[i].kind != ns[0].kind || ns[i].op != ns[0].op {
			return false
		}
	}
	switch ns[0].kind {
	case vsLoad:
		for _, id := range l {
			lane[id] = true
		}
		return true
	case vsBin:
		var ds [4]int32
		isAbs := true
		for i, id := range l {
			var ok bool
			if ds[i], ok = s.abs(id); !ok {
				isAbs = false
				break
			}
		}
		if isAbs {
			return s.lanesOK(ds, lane)
		}
		a, b := [4]int32{}, [4]int32{}
		for i := range ns {
			a[i], b[i] = ns[i].a, ns[i].b
		}
		switch ns[0].op {
		case wasm.OpcodeI32Shl, wasm.OpcodeI32ShrS, wasm.OpcodeI32ShrU:
			if !(b[0] == b[1] && b[0] == b[2] && b[0] == b[3]) || s.hasLoad(b[0]) {
				return false
			}
			return s.lanesOK(a, lane)
		}
		return s.lanesOK(a, lane) && s.lanesOK(b, lane)
	}
	return false // Constants or locals that differ from lane to lane.
}

// vecSum is the vector sum being lowered: from the instruction after its vector code, its own lowering
// runs up to end, and join takes either total.
type vecSum struct {
	scanned int
	cands   []vsCand
	join    ssa.BasicBlock
	end     int
}

type vsCand struct{ start, end int }

// scanVecSums finds the runs from pc on that may be vector sums, the longest where they overlap.
func (c *Compiler) scanVecSums(pc int) {
	vs := &c.vecSum
	body := c.wasmFunctionBody
	s := newVsSim()
	var cands []vsCand
	at := pc
	for ops := 0; at < len(body) && ops < vecSumMaxOps; ops++ {
		next, ok := s.step(body, at)
		if !ok {
			break
		}
		if body[at] == wasm.OpcodeI32Add && (next >= len(body) || body[next] != wasm.OpcodeI32Add) {
			top := s.stack[len(s.stack)-1]
			if s.plan(top.id, s.loadsUnder(top.id)) != nil {
				cands = append(cands, vsCand{start: top.start, end: next})
			}
		}
		at = next
	}
	vs.scanned = max(at, pc+1)
	slices.SortFunc(cands, func(x, y vsCand) int {
		if x.start != y.start {
			return cmp.Compare(x.start, y.start)
		}
		return cmp.Compare(y.end, x.end)
	})
	vs.cands = vs.cands[:0]
	for _, cd := range cands {
		if n := len(vs.cands); n == 0 || cd.start >= vs.cands[n-1].end {
			vs.cands = append(vs.cands, cd)
		}
	}
}

// lowerVecSums is called before each instruction of the function body: it starts a vector sum where one
// begins, and joins its two lowerings where it ends.
func (c *Compiler) lowerVecSums() {
	vs := &c.vecSum
	state := c.state()
	pc := state.pc
	if vs.join != nil {
		if pc == vs.end {
			c.joinVecSum()
		}
		return
	}
	if state.unreachable || !c.vecMap || !c.vecMapMemory() || c.inlinedCallee != 0 || c.multiMemory {
		return
	}
	if pc >= vs.scanned {
		c.scanVecSums(pc)
	}
	for len(vs.cands) > 0 && vs.cands[0].start < pc {
		vs.cands = vs.cands[1:]
	}
	if len(vs.cands) == 0 || vs.cands[0].start != pc {
		return
	}
	cd := vs.cands[0]
	vs.cands = vs.cands[1:]
	s := newVsSim()
	for at := pc; at < cd.end; {
		next, ok := s.step(c.wasmFunctionBody, at)
		if !ok {
			return
		}
		at = next
	}
	if len(s.stack) != 1 {
		return
	}
	if p := s.plan(s.stack[0].id, s.loads); p != nil {
		if VecSumHook != nil {
			VecSumHook(c.wasmLocalFunctionIndex, pc)
		}
		c.startVecSum(s, p, cd.end)
	}
}

// startVecSum emits the checks and the vector code of the vector sum s plans as p, and leaves the
// current block where its own lowering goes.
func (c *Compiler) startVecSum(s *vsSim, p *vsPlan, end int) {
	builder := c.ssaBuilder
	ins := func() *ssa.Instruction { return builder.AllocateInstruction() }
	scalars := map[int32]ssa.Value{}
	streamOf := func(id int32) (*vsStream, uint32) {
		n := s.nodes[id]
		base, c := s.vsAddr(n.a)
		for i := range p.streams {
			if st := &p.streams[i]; st.base == base {
				return st, uint32(st.at(c, n.k))
			}
		}
		panic("BUG: a vector sum load in no row")
	}
	var scalar func(id int32) ssa.Value
	scalar = func(id int32) ssa.Value {
		if v, ok := scalars[id]; ok {
			return v
		}
		n := s.nodes[id]
		var v ssa.Value
		switch n.kind {
		case vsEntry:
			v = builder.MustFindValue(c.localVariable(uint32(n.k)))
		case vsConst:
			v = ins().AsIconst32(uint32(n.k)).Insert(builder).Return()
		case vsLoad:
			st, at := streamOf(id)
			v = ins().AsLoad(st.addr, at, ssa.TypeI32).Insert(builder).Return()
		default:
			x, y := scalar(n.a), scalar(n.b)
			i := ins()
			switch n.op {
			case wasm.OpcodeI32Add:
				i.AsIadd(x, y)
			case wasm.OpcodeI32Sub:
				i.AsIsub(x, y)
			case wasm.OpcodeI32Mul:
				i.AsImul(x, y)
			case wasm.OpcodeI32And:
				i.AsBand(x, y)
			case wasm.OpcodeI32Or:
				i.AsBor(x, y)
			case wasm.OpcodeI32Xor:
				i.AsBxor(x, y)
			case wasm.OpcodeI32Shl:
				i.AsIshl(x, y)
			case wasm.OpcodeI32ShrS:
				i.AsSshr(x, y)
			default:
				i.AsUshr(x, y)
			}
			v = i.Insert(builder).Return()
		}
		scalars[id] = v
		return v
	}

	// Every row's elements are in bounds where its end is: an i32 address computation reaching past
	// 4 GiB would have wrapped, but it would then have left any memory first. So each element is where
	// the row's start, plus its own constants, puts it.
	memBase := c.getMemoryBaseValue(0, false)
	memLen := c.getMemoryLenValue(0, false)
	oob := ssa.ValueInvalid
	for i := range p.streams {
		st := &p.streams[i]
		lo := ins().AsIadd(scalar(st.base), ins().AsIconst32(uint32(st.lo)).Insert(builder).Return()).Insert(builder).Return()
		ext := ins().AsUExtend(lo, 32, 64).Insert(builder).Return()
		hi := ins().AsIadd(ext, ins().AsIconst64(st.end).Insert(builder).Return()).Insert(builder).Return()
		out := ins().AsIcmp(hi, memLen, ssa.IntegerCmpCondUnsignedGreaterThan).Insert(builder).Return()
		if oob.Valid() {
			i := ins()
			i.AsBor(oob, out)
			out = i.Insert(builder).Return()
		}
		oob = out
		st.addr = ins().AsIadd(memBase, ext).Insert(builder).Return()
	}
	blk := builder.CurrentBlock()
	fast, slow, join := builder.AllocateBasicBlock(), builder.AllocateBasicBlock(), builder.AllocateBasicBlock()
	join.AddParam(builder, ssa.TypeI32)
	builder.AllocateInstruction().AsBrnz(oob, ssa.ValuesNil, slow).Insert(builder)
	c.insertJumpToBlock(ssa.ValuesNil, fast)
	c.finalizeKnownSafeBoundsAtTheEndOfBlock(blk.ID())

	builder.SetCurrentBlock(fast)
	builder.Seal(fast)
	c.initializeCurrentBlockKnownBounds()
	vecs := map[[4]int32]ssa.Value{}
	var vec func(l [4]int32) ssa.Value
	vec = func(l [4]int32) ssa.Value {
		if v, ok := vecs[l]; ok {
			return v
		}
		var v ssa.Value
		n := s.nodes[l[0]]
		if l[0] == l[1] && l[0] == l[2] && l[0] == l[3] {
			v = ins().AsSplat(scalar(l[0]), ssa.VecLaneI32x4).Insert(builder).Return()
		} else if n.kind == vsLoad {
			st, at := streamOf(l[0])
			v = ins().AsLoad(st.addr, at, ssa.TypeV128).Insert(builder).Return()
		} else if _, ok := s.abs(l[0]); ok {
			var ds [4]int32
			for i, id := range l {
				ds[i], _ = s.abs(id)
			}
			v = ins().AsVIabs(vec(ds), ssa.VecLaneI32x4).Insert(builder).Return()
		} else {
			var a, b [4]int32
			for i, id := range l {
				a[i], b[i] = s.nodes[id].a, s.nodes[id].b
			}
			const lane = ssa.VecLaneI32x4
			switch n.op {
			case wasm.OpcodeI32Shl:
				v = ins().AsVIshl(vec(a), scalar(b[0]), lane).Insert(builder).Return()
			case wasm.OpcodeI32ShrS:
				v = ins().AsVSshr(vec(a), scalar(b[0]), lane).Insert(builder).Return()
			case wasm.OpcodeI32ShrU:
				v = ins().AsVUshr(vec(a), scalar(b[0]), lane).Insert(builder).Return()
			case wasm.OpcodeI32Add:
				v = ins().AsVIadd(vec(a), vec(b), lane).Insert(builder).Return()
			case wasm.OpcodeI32Sub:
				v = ins().AsVIsub(vec(a), vec(b), lane).Insert(builder).Return()
			case wasm.OpcodeI32Mul:
				v = ins().AsVImul(vec(a), vec(b), lane).Insert(builder).Return()
			case wasm.OpcodeI32And:
				v = ins().AsVband(vec(a), vec(b)).Insert(builder).Return()
			case wasm.OpcodeI32Or:
				v = ins().AsVbor(vec(a), vec(b)).Insert(builder).Return()
			default:
				v = ins().AsVbxor(vec(a), vec(b)).Insert(builder).Return()
			}
		}
		vecs[l] = v
		return v
	}
	acc := ssa.ValueInvalid
	for _, g := range p.groups {
		v := vec(g)
		if acc.Valid() {
			v = ins().AsVIadd(acc, v, ssa.VecLaneI32x4).Insert(builder).Return()
		}
		acc = v
	}
	sum := ins().AsVIaddReduce(acc).Insert(builder).Return()
	for _, t := range p.rest {
		sum = ins().AsIadd(sum, scalar(t)).Insert(builder).Return()
	}
	// The locals the run sets, all computed before any is, since each is computed from the run's start.
	idx := make([]uint32, 0, len(s.locals))
	for k, id := range s.locals {
		if n := s.nodes[id]; n.kind != vsEntry || n.k != uint64(k) {
			idx = append(idx, k)
		}
	}
	slices.Sort(idx)
	vals := make([]ssa.Value, len(idx))
	for i, k := range idx {
		vals[i] = scalar(s.locals[k])
	}
	for i, k := range idx {
		builder.DefineVariableInCurrentBB(c.localVariable(k), vals[i])
	}
	c.insertJumpToBlock(c.allocateVarLengthValues(1, sum), join)
	c.finalizeKnownSafeBoundsAtTheEndOfBlock(fast.ID())

	builder.SetCurrentBlock(slow)
	builder.Seal(slow)
	c.initializeCurrentBlockKnownBounds()
	c.vecSum.join, c.vecSum.end = join, end
}

// joinVecSum ends the vector sum's own lowering, with its total on the stack, at its join.
func (c *Compiler) joinVecSum() {
	builder := c.ssaBuilder
	state := c.state()
	join := c.vecSum.join
	c.vecSum.join = nil
	c.insertJumpToBlock(c.allocateVarLengthValues(1, state.pop()), join)
	c.finalizeKnownSafeBoundsAtTheEndOfBlock(builder.CurrentBlock().ID())
	builder.SetCurrentBlock(join)
	builder.Seal(join)
	c.initializeCurrentBlockKnownBounds()
	state.push(join.Param(0))
}
