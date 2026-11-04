package wasm

import (
	"math/rand"
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// solveEntryChecks builds an entryCheckBuilder for local functions of the given
// body sizes, with calls given as caller -> callee pairs of local indices plus
// imports.
func solveEntryChecks(imports uint32, sizes []uint32, calls [][2]uint32, indirect []uint32, referenced []Index) []bool {
	needs, _ := solveCallWork(imports, sizes, calls, indirect, referenced, nil)
	return needs
}

// solveCallWork is solveEntryChecks that also returns the call work, with the
// local functions in unbounded marked as having a loop.
func solveCallWork(imports uint32, sizes []uint32, calls [][2]uint32, indirect []uint32, referenced []Index, unbounded []Index) ([]bool, []uint32) {
	b := &entryCheckBuilder{imports: imports, size: sizes, indirect: make([]uint32, len(sizes)), unbounded: make([]bool, len(sizes))}
	for _, f := range unbounded {
		b.markUnbounded(f)
	}
	for _, c := range calls {
		b.call(c[0], c[1])
	}
	for _, f := range indirect {
		b.callIndirect(f)
	}
	refs := map[Index]struct{}{}
	for _, r := range referenced {
		refs[r] = struct{}{}
	}
	return b.solve(refs)
}

func TestEntryCheckBuilder(t *testing.T) {
	small := uint32(100)
	t.Run("no calls", func(t *testing.T) {
		require.Equal(t, []bool{false, false}, solveEntryChecks(0, []uint32{small, small}, nil, nil, nil))
	})
	t.Run("self recursion", func(t *testing.T) {
		require.Equal(t, []bool{true, false}, solveEntryChecks(0, []uint32{small, small}, [][2]uint32{{0, 0}, {1, 0}}, nil, nil))
	})
	t.Run("mutual recursion", func(t *testing.T) {
		require.Equal(t, []bool{true, true, false},
			solveEntryChecks(0, []uint32{small, small, small}, [][2]uint32{{0, 1}, {1, 0}, {2, 0}}, nil, nil))
	})
	t.Run("call to an import", func(t *testing.T) {
		// Function index 0 is the import; local 0 calls it, which checks first.
		require.Equal(t, []bool{false}, solveEntryChecks(1, []uint32{small}, [][2]uint32{{0, 0}}, nil, nil))
	})
	t.Run("indirect call that may leave the module", func(t *testing.T) {
		// As "recursion through an indirect call", but the call checks first.
		b := &entryCheckBuilder{size: []uint32{small, small}, indirect: make([]uint32, 2), unbounded: make([]bool, 2), foreign: true}
		b.callIndirect(0)
		needs, work := b.solve(map[Index]struct{}{0: {}})
		require.Equal(t, []bool{false, false}, needs)
		require.Equal(t, []uint32{0, small}, work)
	})
	t.Run("recursion through an indirect call", func(t *testing.T) {
		// Local 0 makes an indirect call and its own reference is taken.
		require.Equal(t, []bool{true, false},
			solveEntryChecks(0, []uint32{small, small}, nil, []uint32{0}, []Index{0}))
	})
	t.Run("indirect call that cannot recur", func(t *testing.T) {
		// Local 0 calls indirectly, but only local 1, which calls nothing, is referenced.
		require.Equal(t, []bool{false, false},
			solveEntryChecks(0, []uint32{small, small}, nil, []uint32{0}, []Index{1}))
	})
	t.Run("cycles through the hub", func(t *testing.T) {
		// Every local is referenced and reaches local 0, which calls indirectly, so all
		// are on cycles through the hub, which checking local 0 breaks. Locals 1 and 2
		// also call each other, a cycle of their own, and local 3 is on no other.
		require.Equal(t, []bool{true, true, true, false},
			solveEntryChecks(0, []uint32{small, small, small, small},
				[][2]uint32{{1, 2}, {2, 1}, {2, 0}, {3, 0}}, []uint32{0}, []Index{0, 1, 2, 3}))
	})
	t.Run("indirect call that may be heavy", func(t *testing.T) {
		// Of what local 0's indirect call can reach, only local 1 is over the budget,
		// so only it is checked.
		require.Equal(t, []bool{false, true, false},
			solveEntryChecks(0, []uint32{small, PreemptBudget + 1, small}, nil, []uint32{0}, []Index{1, 2}))
	})
	t.Run("many indirect calls", func(t *testing.T) {
		// 1000 calls to local 1 would pass the budget and 1000 to local 2 would not,
		// so local 1 is checked and local 2 is not.
		indirect := make([]uint32, 1000)
		require.Equal(t, []bool{false, true, false},
			solveEntryChecks(0, []uint32{small, small, small / 2}, nil, indirect, []Index{1, 2}))
	})
	t.Run("small call tree", func(t *testing.T) {
		// 0 calls 1 twice, 1 calls 2 twice: 7 bodies' work, well within the budget.
		require.Equal(t, []bool{false, false, false},
			solveEntryChecks(0, []uint32{small, small, small}, [][2]uint32{{0, 1}, {0, 1}, {1, 2}, {1, 2}}, nil, nil))
	})
	t.Run("exponential call tree", func(t *testing.T) {
		// Each of 20 functions calls the next twice: 2^20 bodies' work, so some
		// have to be checked, and every function's unchecked work stays bounded.
		const n = 20
		sizes := make([]uint32, n)
		var calls [][2]uint32
		for i := range sizes {
			sizes[i] = small
			if i+1 < n {
				calls = append(calls, [2]uint32{uint32(i), uint32(i + 1)}, [2]uint32{uint32(i), uint32(i + 1)})
			}
		}
		needs := solveEntryChecks(0, sizes, calls, nil, nil)
		checked := 0
		for _, c := range needs {
			if c {
				checked++
			}
		}
		require.True(t, checked > 0 && checked < n, "checked %d of %d", checked, n)
		// The unchecked work from any function to the next check is at most the
		// budget: walk down the chain doubling until a checked function.
		for i := 0; i < n; i++ {
			work, mult := uint64(0), uint64(1)
			for j := i; j < n; j++ {
				if j > i && needs[j] {
					break
				}
				work += mult * uint64(sizes[j])
				mult *= 2
			}
			require.True(t, work <= uint64(sizes[i])+PreemptBudget, "function %d does %d unchecked", i, work)
		}
	})
	t.Run("one large callee", func(t *testing.T) {
		// A callee whose body alone is over the budget is checked rather than its caller.
		require.Equal(t, []bool{false, true},
			solveEntryChecks(0, []uint32{small, PreemptBudget + 1}, [][2]uint32{{0, 1}}, nil, nil))
	})
	t.Run("large caller", func(t *testing.T) {
		// The caller's own body is over the budget, which checking its callee would
		// not change, and the calls alone are within it.
		require.Equal(t, []bool{false, false},
			solveEntryChecks(0, []uint32{PreemptBudget, small}, [][2]uint32{{0, 1}, {0, 1}}, nil, nil))
	})
}

func TestCallWork(t *testing.T) {
	small := uint32(100)
	t.Run("call tree", func(t *testing.T) {
		// 0 calls 1 twice, 1 calls 2 once.
		_, work := solveCallWork(0, []uint32{small, small, small}, [][2]uint32{{0, 1}, {0, 1}, {1, 2}}, nil, nil, nil)
		require.Equal(t, []uint32{5 * small, 2 * small, small}, work)
	})
	t.Run("a loop below", func(t *testing.T) {
		// 1 has a loop, so neither it nor its caller is short; 2 still is.
		_, work := solveCallWork(0, []uint32{small, small, small}, [][2]uint32{{0, 1}, {0, 2}}, nil, nil, []Index{1})
		require.Equal(t, []uint32{0, 0, small}, work)
	})
	t.Run("call to an import", func(t *testing.T) {
		_, work := solveCallWork(1, []uint32{small, small}, [][2]uint32{{0, 0}}, nil, nil, nil)
		require.Equal(t, []uint32{0, small}, work)
	})
	t.Run("recursion", func(t *testing.T) {
		_, work := solveCallWork(0, []uint32{small, small, small}, [][2]uint32{{0, 0}, {1, 2}, {2, 1}}, nil, nil, nil)
		require.Equal(t, []uint32{0, 0, 0}, work)
	})
	t.Run("indirect call", func(t *testing.T) {
		_, work := solveCallWork(0, []uint32{small, small}, nil, []uint32{0}, []Index{1}, nil)
		require.Equal(t, []uint32{0, small}, work)
	})
	t.Run("over the budget", func(t *testing.T) {
		// 0 calls 1 twice, which is half the budget plus a byte.
		_, work := solveCallWork(0, []uint32{1, PreemptBudget/2 + 1}, [][2]uint32{{0, 1}, {0, 1}}, nil, nil, nil)
		require.Equal(t, []uint32{0, PreemptBudget/2 + 1}, work)
	})
}

func TestTarjan(t *testing.T) {
	// 0 -> 1 -> 2 -> 1, 2 -> 3
	start := []uint32{0, 1, 2, 4, 4}
	succ := []int32{1, 2, 1, 3}
	sccs := tarjan(4, start, succ)
	require.Equal(t, 3, len(sccs))
	require.Equal(t, []int32{3}, sccs[0])
	require.Equal(t, 2, len(sccs[1]))
	require.Equal(t, []int32{0}, sccs[2])
}

// Validation counts the imported functions itself, so a module built by hand,
// whose ImportFunctionCount nothing set, gets the same answer as a decoded one.
func TestModule_Validate_entryChecks(t *testing.T) {
	m := &Module{
		TypeSection:     []FunctionType{{}},
		ImportSection:   []Import{{Module: "host", Name: "f", Type: ExternTypeFunc, DescFunc: 0}},
		FunctionSection: []Index{0, 0},
		CodeSection: []Code{
			{Body: []byte{OpcodeCall, 0, OpcodeCall, 1, OpcodeEnd}}, // calls the import, then itself
			{Body: []byte{OpcodeCall, 0, OpcodeEnd}},                // calls only the import
		},
	}
	require.NoError(t, m.Validate(api.CoreFeaturesV2))
	require.Equal(t, []bool{true, false}, m.FunctionNeedsEntryCheck)
	require.Equal(t, []uint32{0, 0}, m.FunctionCallWork)
}

// Validation marks the functions with a loop or a bulk operation, and through
// them their callers, as ones whose calls may run long.
func TestModule_Validate_callWork(t *testing.T) {
	m := &Module{
		TypeSection:     []FunctionType{{}},
		FunctionSection: []Index{0, 0, 0, 0},
		MemorySection:   []Memory{{Min: 1, Max: 1}},
		CodeSection: []Code{
			{Body: []byte{OpcodeLoop, 0x40, OpcodeEnd, OpcodeEnd}},
			{Body: []byte{OpcodeI32Const, 0, OpcodeI32Const, 0, OpcodeI32Const, 1, OpcodeMiscPrefix, OpcodeMiscMemoryFill, 0, OpcodeEnd}},
			{Body: []byte{OpcodeCall, 0, OpcodeEnd}},
			{Body: []byte{OpcodeNop, OpcodeEnd}},
		},
	}
	require.NoError(t, m.Validate(api.CoreFeaturesV2))
	require.Equal(t, []uint32{0, 0, 0, 2}, m.FunctionCallWork)
}

// A module's indirect calls may reach another module's function when a reference
// to one can get in: see Module.foreignFuncRefs.
func TestModule_foreignFuncRefs(t *testing.T) {
	funcref := FunctionType{Params: []ValueType{ValueTypeFuncref}}
	for _, tc := range []struct {
		name    string
		m       *Module
		foreign bool
	}{
		{name: "numbers only", m: &Module{
			ImportSection: []Import{{Type: ExternTypeFunc, DescFunc: 0}},
			ExportSection: []Export{{Type: ExternTypeFunc, Index: 1}},
		}},
		{name: "an imported function in a table", m: &Module{
			ImportSection: []Import{{Type: ExternTypeFunc, DescFunc: 0}},
			ElementSection: []ElementSegment{{
				OffsetExpr: NewConstantExpressionFromI32(0), Init: []Index{0}, Type: RefTypeFuncref, Mode: ElementModeActive,
			}},
		}, foreign: true},
		{name: "an imported table", m: &Module{
			ImportSection: []Import{{Type: ExternTypeTable, DescTable: Table{Type: RefTypeFuncref}}},
		}, foreign: true},
		{name: "an exported table", m: &Module{
			TableSection:  []Table{{Type: RefTypeFuncref}},
			ExportSection: []Export{{Type: ExternTypeTable, Index: 0}},
		}, foreign: true},
		{name: "an import that takes a reference", m: &Module{
			ImportSection: []Import{{Type: ExternTypeFunc, DescFunc: 1}},
		}, foreign: true},
		{name: "an export that takes a reference", m: &Module{
			FunctionSection: []Index{0, 1},
			ExportSection:   []Export{{Type: ExternTypeFunc, Index: 1}},
		}, foreign: true},
		{name: "an imported global reference", m: &Module{
			ImportSection: []Import{{Type: ExternTypeGlobal, DescGlobal: GlobalType{ValType: ValueTypeFuncref}}},
		}, foreign: true},
		{name: "an exported global reference", m: &Module{
			GlobalSection: []Global{{Type: GlobalType{ValType: ValueTypeExternref}, Init: ConstantExpression{Data: []byte{OpcodeRefNull, byte(ValueTypeExternref), OpcodeEnd}}}},
			ExportSection: []Export{{Type: ExternTypeGlobal, Index: 0}},
		}, foreign: true},
		{name: "an imported tag that carries a reference", m: &Module{
			ImportSection: []Import{{Type: ExternTypeTag, DescTag: 1}},
		}, foreign: true},
		{name: "an export out of range", m: &Module{
			ExportSection: []Export{{Type: ExternTypeFunc, Index: 5}},
		}, foreign: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.m
			m.TypeSection = []FunctionType{{}, funcref}
			for _, imp := range m.ImportSection {
				if imp.Type == ExternTypeFunc {
					m.ImportFunctionCount++
				}
			}
			if m.FunctionSection == nil {
				m.FunctionSection = []Index{0}
			}
			functions, globals, _, _, tags, err := m.AllDeclarations()
			require.NoError(t, err)
			declared, err := m.declaredFunctionIndexes(api.CoreFeaturesV3)
			require.NoError(t, err)
			require.Equal(t, tc.foreign, m.foreignFuncRefs(declared, functions, globals, tags))
		})
	}
}

// TestEntryCheckBuilder_deterministic solves random call graphs with indirect calls many times
// over. The set of referenced functions arrives as a map, whose iteration order changes from one
// range to the next, and the answer must not follow it: compiled code would differ between two
// compilations of the same module.
func TestEntryCheckBuilder_deterministic(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	for g := 0; g < 200; g++ {
		n := 2 + rnd.Intn(30)
		sizes := make([]uint32, n)
		for i := range sizes {
			sizes[i] = uint32(rnd.Intn(PreemptBudget / 4))
		}
		var calls [][2]uint32
		for i := rnd.Intn(3 * n); i > 0; i-- {
			calls = append(calls, [2]uint32{uint32(rnd.Intn(n)), uint32(rnd.Intn(n))})
		}
		var indirect, referenced, unbounded []Index
		for f := 0; f < n; f++ {
			if rnd.Intn(4) == 0 {
				indirect = append(indirect, Index(f))
			}
			if rnd.Intn(2) == 0 {
				referenced = append(referenced, Index(f))
			}
			if rnd.Intn(8) == 0 {
				unbounded = append(unbounded, Index(f))
			}
		}
		needs, work := solveCallWork(0, sizes, calls, indirect, referenced, unbounded)
		for i := 0; i < 20; i++ {
			n2, w2 := solveCallWork(0, sizes, calls, indirect, referenced, unbounded)
			require.Equal(t, needs, n2, "graph %d", g)
			require.Equal(t, work, w2, "graph %d", g)
		}
	}
}
