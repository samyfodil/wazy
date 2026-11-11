package frontend

import (
	"slices"
	"strings"
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

// preemptChecks lowers the last local function of m and reports, for its loop
// checks, how many are kept and how many elided, and whether it keeps an entry check.
func preemptChecks(t *testing.T, m *wasm.Module, features api.CoreFeatures) (kept, elided int, entry bool) {
	for _, blk := range strings.Split(strings.TrimSpace(lowerLast(t, m, features)), "\n\n") {
		for _, line := range strings.Split(blk, "\n") {
			if !strings.Contains(line, "= PreemptRequested") {
				continue
			}
			isElided := strings.HasSuffix(line, "PreemptRequested elided")
			if strings.HasPrefix(blk, "blk0:") {
				entry = !isElided
				continue
			}
			if isElided {
				elided++
			} else {
				kept++
			}
		}
	}
	return
}

// lowerLast lowers the last local function of m and returns its SSA.
func lowerLast(t *testing.T, m *wasm.Module, features api.CoreFeatures) string {
	require.NoError(t, m.Validate(features))
	b := ssa.NewBuilder()
	offset := nativeapi.NewModuleContextOffsetData(m, false)
	fc := NewFrontendCompiler(m, b, &offset, false, false)
	idx := wasm.Index(len(m.CodeSection) - 1)
	typeIndex := m.FunctionSection[idx]
	code := &m.CodeSection[idx]
	fc.Init(idx, typeIndex, &m.TypeSection[typeIndex], code.LocalTypes, code.Body, false, 0)
	fc.LowerToSSA()
	return fc.formatBuilder()
}

func TestPreemptCheckPlacement(t *testing.T) {
	// The import is function 0; g, which calls itself and so is checked on entry,
	// is 1; and the function under test is 2.
	const imp, g, self = 0, 1, 2
	const i32 = wasm.ValueTypeI32
	const blockI32 = byte(i32) // as a block type
	for _, tc := range []struct {
		name            string
		params, results []wasm.ValueType
		locals          []wasm.ValueType
		body            []byte
		features        api.CoreFeatures
		kept, elided    int
		entry           bool
	}{
		{
			// TinyGo's fibonacci: the loop's every way back passes the recursive call,
			// which is checked on entry, so only the entry check is kept.
			name:   "loop covered by a checked call in an else",
			params: []wasm.ValueType{i32}, results: []wasm.ValueType{i32}, locals: []wasm.ValueType{i32},
			body: []byte{
				wasm.OpcodeLoop, blockI32,
				wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Const, 1, wasm.OpcodeI32LeU,
				wasm.OpcodeIf, blockI32,
				wasm.OpcodeLocalGet, 0, wasm.OpcodeLocalGet, 1, wasm.OpcodeI32Add,
				wasm.OpcodeElse,
				wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Const, 1, wasm.OpcodeI32Sub, wasm.OpcodeCall, self,
				wasm.OpcodeLocalGet, 1, wasm.OpcodeI32Add, wasm.OpcodeLocalSet, 1,
				wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Const, 2, wasm.OpcodeI32Sub, wasm.OpcodeLocalSet, 0,
				wasm.OpcodeBr, 1,
				wasm.OpcodeEnd,
				wasm.OpcodeEnd,
				wasm.OpcodeEnd,
			},
			elided: 1, entry: true,
		},
		{
			name:   "loop covered by a call to a checked function",
			body:   []byte{wasm.OpcodeLoop, 0x40, wasm.OpcodeCall, g, wasm.OpcodeBr, 0, wasm.OpcodeEnd, wasm.OpcodeEnd},
			elided: 1,
		},
		{
			// The import may be another module's compiled function, which runs
			// without returning to Go, so the call checks first.
			name: "a call to an import is checked first",
			body: []byte{wasm.OpcodeLoop, 0x40, wasm.OpcodeCall, imp, wasm.OpcodeBr, 0, wasm.OpcodeEnd, wasm.OpcodeEnd},
			kept: 1, elided: 1,
		},
		{
			name:   "a way back before the call",
			params: []wasm.ValueType{i32},
			body: []byte{
				wasm.OpcodeLoop, 0x40,
				wasm.OpcodeLocalGet, 0, wasm.OpcodeBrIf, 0,
				wasm.OpcodeCall, g, wasm.OpcodeBr, 0,
				wasm.OpcodeEnd, wasm.OpcodeEnd,
			},
			kept: 1,
		},
		{
			name:   "the call in one arm of an if",
			params: []wasm.ValueType{i32},
			body: []byte{
				wasm.OpcodeLoop, 0x40,
				wasm.OpcodeLocalGet, 0, wasm.OpcodeIf, 0x40, wasm.OpcodeCall, g, wasm.OpcodeEnd,
				wasm.OpcodeBr, 0,
				wasm.OpcodeEnd, wasm.OpcodeEnd,
			},
			kept: 1,
		},
		{
			name:   "the call in both arms of an if",
			params: []wasm.ValueType{i32},
			body: []byte{
				wasm.OpcodeLoop, 0x40,
				wasm.OpcodeLocalGet, 0, wasm.OpcodeIf, 0x40, wasm.OpcodeCall, g, wasm.OpcodeElse, wasm.OpcodeCall, g, wasm.OpcodeEnd,
				wasm.OpcodeBr, 0,
				wasm.OpcodeEnd, wasm.OpcodeEnd,
			},
			elided: 1,
		},
		{
			name: "the call in a nested block",
			body: []byte{
				wasm.OpcodeLoop, 0x40,
				wasm.OpcodeBlock, 0x40, wasm.OpcodeCall, g, wasm.OpcodeEnd,
				wasm.OpcodeBr, 0,
				wasm.OpcodeEnd, wasm.OpcodeEnd,
			},
			elided: 1,
		},
		{
			name:   "a branch out of the block around the call",
			params: []wasm.ValueType{i32},
			body: []byte{
				wasm.OpcodeLoop, 0x40,
				wasm.OpcodeBlock, 0x40, wasm.OpcodeLocalGet, 0, wasm.OpcodeBrIf, 0, wasm.OpcodeCall, g, wasm.OpcodeEnd,
				wasm.OpcodeBr, 0,
				wasm.OpcodeEnd, wasm.OpcodeEnd,
			},
			kept: 1,
		},
		{
			// A loop that never branches back runs its body once.
			name:   "no way back",
			params: []wasm.ValueType{i32},
			body:   []byte{wasm.OpcodeLoop, 0x40, wasm.OpcodeEnd, wasm.OpcodeLocalGet, 0, wasm.OpcodeCall, self, wasm.OpcodeEnd},
			elided: 1, entry: true,
		},
		{
			// The function recurses, so it needs a check on entry, but the loop it
			// opens with keeps a full one, which every call runs first.
			name:   "leading loop stands in for the entry check",
			params: []wasm.ValueType{i32},
			body: []byte{
				wasm.OpcodeBlock, 0x40, wasm.OpcodeNop,
				wasm.OpcodeLoop, 0x40, wasm.OpcodeLocalGet, 0, wasm.OpcodeBrIf, 0, wasm.OpcodeEnd,
				wasm.OpcodeEnd,
				wasm.OpcodeLocalGet, 0, wasm.OpcodeCall, self,
				wasm.OpcodeEnd,
			},
			kept: 1,
		},
		{
			// Every way round the outer loop goes through the inner loop's header,
			// whose full check covers it, whether it leaves the inner loop by a
			// branch or from its end.
			name:   "an inner loop entered every time round",
			params: []wasm.ValueType{i32},
			body: []byte{
				wasm.OpcodeLoop, 0x40,
				wasm.OpcodeBlock, 0x40,
				wasm.OpcodeLoop, 0x40,
				wasm.OpcodeLocalGet, 0, wasm.OpcodeBrIf, 1,
				wasm.OpcodeLocalGet, 0, wasm.OpcodeBrIf, 0,
				wasm.OpcodeEnd,
				wasm.OpcodeEnd,
				wasm.OpcodeBr, 0,
				wasm.OpcodeEnd, wasm.OpcodeEnd,
			},
			kept: 1, elided: 1,
		},
		{
			name:   "an inner loop entered some of the time",
			params: []wasm.ValueType{i32},
			body: []byte{
				wasm.OpcodeLoop, 0x40,
				wasm.OpcodeLocalGet, 0, wasm.OpcodeIf, 0x40,
				wasm.OpcodeLoop, 0x40, wasm.OpcodeLocalGet, 0, wasm.OpcodeBrIf, 0, wasm.OpcodeEnd,
				wasm.OpcodeEnd,
				wasm.OpcodeBr, 0,
				wasm.OpcodeEnd, wasm.OpcodeEnd,
			},
			kept: 2,
		},
		{
			// The inner loop's check is elided, since its way back calls g, so the
			// branch out of it, which does not, leaves the outer loop its check.
			name:   "a branch out of an inner loop with no check",
			params: []wasm.ValueType{i32},
			body: []byte{
				wasm.OpcodeLoop, 0x40,
				wasm.OpcodeBlock, 0x40,
				wasm.OpcodeLoop, 0x40,
				wasm.OpcodeLocalGet, 0, wasm.OpcodeBrIf, 1,
				wasm.OpcodeCall, g, wasm.OpcodeBr, 0,
				wasm.OpcodeEnd,
				wasm.OpcodeEnd,
				wasm.OpcodeBr, 0,
				wasm.OpcodeEnd, wasm.OpcodeEnd,
			},
			kept: 1, elided: 1,
		},
		{
			// The branch out of the innermost loop, whose check is elided, also
			// leaves the middle one, whose full check covers it and the outer loop.
			name:   "a branch out of two loops",
			params: []wasm.ValueType{i32},
			body: []byte{
				wasm.OpcodeLoop, 0x40,
				wasm.OpcodeBlock, 0x40,
				wasm.OpcodeLoop, 0x40,
				wasm.OpcodeLoop, 0x40,
				wasm.OpcodeLocalGet, 0, wasm.OpcodeBrIf, 2,
				wasm.OpcodeLocalGet, 0, wasm.OpcodeIf, 0x40, wasm.OpcodeCall, g, wasm.OpcodeBr, 1, wasm.OpcodeEnd,
				wasm.OpcodeEnd,
				wasm.OpcodeLocalGet, 0, wasm.OpcodeBrIf, 0,
				wasm.OpcodeEnd,
				wasm.OpcodeEnd,
				wasm.OpcodeBr, 0,
				wasm.OpcodeEnd, wasm.OpcodeEnd,
			},
			kept: 1, elided: 2,
		},
		{
			// A throw can land the loop's catch from anywhere in the try body:
			// here from the import, before the call that would cover the loop.
			name: "a catch that branches back",
			body: []byte{
				wasm.OpcodeLoop, 0x40,
				wasm.OpcodeTryTable, 0x40, 1, wasm.CatchKindCatchAll, 0,
				wasm.OpcodeCall, imp,
				wasm.OpcodeCall, g,
				wasm.OpcodeEnd,
				wasm.OpcodeBr, 0,
				wasm.OpcodeEnd, wasm.OpcodeEnd,
			},
			features: api.CoreFeaturesV2 | api.CoreFeatureExceptionHandling,
			kept:     2, // the loop's, and the import's
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &wasm.Module{
				TypeSection: []wasm.FunctionType{
					{},
					{Params: tc.params, Results: tc.results, ParamNumInUint64: len(tc.params), ResultNumInUint64: len(tc.results)},
				},
				ImportSection:       []wasm.Import{{Module: "host", Name: "f", Type: wasm.ExternTypeFunc, DescFunc: 0}},
				ImportFunctionCount: 1,
				FunctionSection:     []wasm.Index{0, 1},
				CodeSection: []wasm.Code{
					{Body: []byte{wasm.OpcodeCall, g, wasm.OpcodeEnd}},
					{LocalTypes: tc.locals, Body: tc.body},
				},
			}
			features := tc.features
			if features == 0 {
				features = api.CoreFeaturesV2
			}
			kept, elided, entry := preemptChecks(t, m, features)
			require.Equal(t, tc.kept, kept, "loop checks kept")
			require.Equal(t, tc.elided, elided, "loop checks elided")
			require.Equal(t, tc.entry, entry, "entry check kept")
		})
	}
}

// A loop that jumps on a local, as Go lays out a function, checks only on the
// branches back that may land at or behind themselves.
func TestPreemptCheckDispatch(t *testing.T) {
	// A loop that jumps on local 0 to the end of one of two blocks, then runs seg0
	// and seg1 in turn.
	dispatch := func(seg0 []byte, seg1 ...byte) []byte {
		body := []byte{
			wasm.OpcodeLoop, 0x40, wasm.OpcodeBlock, 0x40, wasm.OpcodeBlock, 0x40,
			wasm.OpcodeLocalGet, 0, wasm.OpcodeBrTable, 1, 0, 1,
			wasm.OpcodeEnd,
		}
		body = append(append(body, seg0...), wasm.OpcodeEnd)
		return append(append(body, seg1...), wasm.OpcodeEnd, wasm.OpcodeEnd)
	}
	jump := func(to, depth byte) []byte {
		return []byte{wasm.OpcodeI32Const, to, wasm.OpcodeLocalSet, 0, wasm.OpcodeBr, depth}
	}
	for _, tc := range []struct {
		name string
		body []byte
		kept int
	}{
		{name: "a jump forward", body: dispatch(jump(1, 1))},
		{name: "a jump forward by the default label", body: dispatch(jump(9, 1))},
		{name: "a jump back", body: dispatch(jump(1, 1), jump(0, 0)...), kept: 1},
		{name: "a jump to where it is", body: dispatch(nil, jump(1, 0)...), kept: 1},
		{name: "a jump on a value not known", body: dispatch(nil,
			wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Const, 1, wasm.OpcodeI32Xor, wasm.OpcodeLocalSet, 0, wasm.OpcodeBr, 0,
		), kept: 1},
		{name: "a conditional jump back", body: dispatch(nil,
			wasm.OpcodeI32Const, 0, wasm.OpcodeLocalSet, 0, wasm.OpcodeLocalGet, 0, wasm.OpcodeBrIf, 0,
		), kept: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &wasm.Module{
				TypeSection:     []wasm.FunctionType{{Params: []wasm.ValueType{wasm.ValueTypeI32}, ParamNumInUint64: 1}},
				FunctionSection: []wasm.Index{0},
				CodeSection:     []wasm.Code{{Body: tc.body}},
			}
			// The header's check is always elided.
			kept, elided, entry := preemptChecks(t, m, api.CoreFeaturesV2)
			require.Equal(t, tc.kept, kept)
			require.Equal(t, 1, elided)
			require.False(t, entry)
		})
	}
}

// A loop whose calls and bulk operations are sure to be short keeps the cheaper
// checks of a loop that makes none, and checks on entry; one that may run long
// gets a full check, unless what makes it long checks first on every way round.
func TestPreemptCheckCallWork(t *testing.T) {
	// The import is function 0; short, which does next to nothing, is 1; long,
	// which loops, is 2; and the function under test, which counts local 0 up to
	// local 1 around the given body, is 3.
	const imp, short, long = 0, 1, 2
	fill := func(n ...byte) []byte {
		return append(append([]byte{wasm.OpcodeI32Const, 0, wasm.OpcodeI32Const, 0}, n...), wasm.OpcodeMiscPrefix, wasm.OpcodeMiscMemoryFill, 0)
	}
	const tuned, full, covered = "tuned", "full", "covered"
	for _, tc := range []struct {
		name, want string
		body       []byte
	}{
		{name: "a call to a short function", body: []byte{wasm.OpcodeCall, short}, want: tuned},
		{name: "a call to a function that loops", body: []byte{wasm.OpcodeCall, long}, want: full},
		{name: "a call to an import", body: []byte{wasm.OpcodeCall, imp}, want: covered},
		{name: "a fill of a constant length", body: fill(wasm.OpcodeI32Const, 16), want: tuned},
		{name: "a fill of a kilobyte", body: fill(wasm.OpcodeI32Const, 0x80, 0x08), want: tuned},
		{name: "a fill of more", body: fill(wasm.OpcodeI32Const, 0x81, 0x08), want: covered},
		{name: "a fill of a variable length", body: fill(wasm.OpcodeLocalGet, 1), want: covered},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := append([]byte{wasm.OpcodeLoop, 0x40}, tc.body...)
			body = append(body,
				wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Const, 1, wasm.OpcodeI32Add, wasm.OpcodeLocalTee, 0,
				wasm.OpcodeLocalGet, 1, wasm.OpcodeI32Ne, wasm.OpcodeBrIf, 0,
				wasm.OpcodeEnd, wasm.OpcodeEnd)
			m := &wasm.Module{
				TypeSection: []wasm.FunctionType{
					{},
					{Params: []wasm.ValueType{wasm.ValueTypeI32, wasm.ValueTypeI32}, ParamNumInUint64: 2},
				},
				ImportSection:       []wasm.Import{{Module: "host", Name: "f", Type: wasm.ExternTypeFunc, DescFunc: 0}},
				ImportFunctionCount: 1,
				MemorySection:       []wasm.Memory{{Min: 1, Max: 1}},
				FunctionSection:     []wasm.Index{0, 0, 1},
				CodeSection: []wasm.Code{
					{Body: []byte{wasm.OpcodeNop, wasm.OpcodeEnd}},
					{Body: []byte{wasm.OpcodeLoop, 0x40, wasm.OpcodeEnd, wasm.OpcodeEnd}},
					{Body: body},
				},
			}
			// The function opens with the loop, so blk0 enters it and blk1 is its
			// header.
			blocks := strings.Split(strings.TrimSpace(lowerLast(t, m, api.CoreFeaturesV2)), "\n\n")
			check := func(prefix string) string {
				for _, blk := range blocks {
					if i := strings.Index(blk, "= PreemptRequested"); strings.HasPrefix(blk, prefix) && i >= 0 {
						line := blk[i:]
						return line[:strings.IndexByte(line, '\n')]
					}
				}
				return ""
			}
			header, entry := check("blk1:"), check("blk0:")
			var got string
			switch {
			case header == "= PreemptRequested" && entry == "":
				got = full
			case header != "= PreemptRequested" && entry == "= PreemptRequested":
				got = tuned
			case header == "= PreemptRequested elided" && entry == "":
				got = covered
			}
			require.Equal(t, tc.want, got, "header %q, entry %q", header, entry)
		})
	}
}

// A loop whose check is elided or made rarer checks once on entry instead, unless
// it is sure to end within uncheckedTripsMax iterations, its first masked test
// runs the full check, or the function checks on entry and its loops' work adds
// up to within preemptBudget.
func TestPreemptCheckOnEntry(t *testing.T) {
	// counted is a loop counting local 1, which starts at zero, up to n, a signed
	// LEB128.
	counted := func(n ...byte) []byte {
		b := []byte{
			wasm.OpcodeLoop, 0x40,
			wasm.OpcodeLocalGet, 1, wasm.OpcodeI32Const, 1, wasm.OpcodeI32Add, wasm.OpcodeLocalTee, 1,
			wasm.OpcodeI32Const,
		}
		return append(append(b, n...), wasm.OpcodeI32LtU, wasm.OpcodeBrIf, 0, wasm.OpcodeEnd)
	}
	// loaded is a loop that counts local 1 up from where it starts, and ends on
	// what it loads, so its check can only be masked.
	loaded := []byte{
		wasm.OpcodeLoop, 0x40,
		wasm.OpcodeLocalGet, 1, wasm.OpcodeI32Const, 1, wasm.OpcodeI32Add, wasm.OpcodeLocalTee, 1,
		wasm.OpcodeI32Load, 2, 0, wasm.OpcodeBrIf, 0,
		wasm.OpcodeEnd,
	}
	reset := []byte{wasm.OpcodeI32Const, 0, wasm.OpcodeLocalSet, 1}
	param := []byte{wasm.OpcodeLocalGet, 0, wasm.OpcodeLocalSet, 1}
	// remainder counts local 1 down from local 0 & 3 to zero, as LLVM finishes an
	// unrolled loop, guarded or not by a branch around it when it starts at zero.
	remainder := func(guarded bool) []byte {
		b := []byte{wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Const, 3, wasm.OpcodeI32And, wasm.OpcodeLocalTee, 1}
		if guarded {
			b = append([]byte{wasm.OpcodeBlock, 0x40}, append(b, wasm.OpcodeI32Eqz, wasm.OpcodeBrIf, 0)...)
		} else {
			b = append(b, wasm.OpcodeDrop)
		}
		b = append(b,
			wasm.OpcodeLoop, 0x40,
			wasm.OpcodeLocalGet, 1, wasm.OpcodeI32Const, 1, wasm.OpcodeI32Sub, wasm.OpcodeLocalTee, 1, wasm.OpcodeBrIf, 0,
			wasm.OpcodeEnd)
		if guarded {
			b = append(b, wasm.OpcodeEnd)
		}
		return b
	}
	recurse := []byte{wasm.OpcodeLocalGet, 0, wasm.OpcodeBrIf, 0, wasm.OpcodeLocalGet, 0, wasm.OpcodeCall, 0}
	cat := func(parts ...[]byte) []byte {
		var b []byte
		for _, p := range parts {
			b = append(b, p...)
		}
		return append(b, wasm.OpcodeEnd)
	}
	for _, tc := range []struct {
		name   string
		body   []byte
		checks int // not elided, the entry block's included
		entry  bool
	}{
		{name: "a loop of uncheckedTripsMax trips", body: cat(reset, counted(uncheckedTripsMax))},
		{name: "a loop of one more", body: cat(reset, counted(uncheckedTripsMax+1)), checks: 1, entry: true},
		{name: "two loops", body: cat(reset, counted(uncheckedTripsMax+1), reset, counted(uncheckedTripsMax+1)), checks: 2, entry: true},
		{
			// The function recurses, so it checks on entry, right before the loop.
			name: "leading loop of a function checked on entry", body: cat(counted(uncheckedTripsMax+1), recurse),
			checks: 1, entry: true,
		},
		{
			name: "a later loop of a function checked on entry", body: cat(counted(uncheckedTripsMax+1), reset, counted(uncheckedTripsMax+1), recurse),
			checks: 1, entry: true,
		},
		{
			// Each runs 4000 trips of about 16 bytes, most of the budget.
			name: "loops past the budget of a function checked on entry", body: cat(counted(0xa0, 0x1f), reset, counted(0xa0, 0x1f), recurse),
			checks: 2, entry: true,
		},
		{name: "a remainder loop", body: cat(remainder(true))},
		{
			// From zero it would wrap round, so it is strip-mined: it checks on the
			// way in, and where it runs out of iterations before its next check.
			name: "a remainder loop entered at zero too", body: cat(remainder(false)),
			checks: 2, entry: true,
		},
		{name: "a masked loop starting at zero", body: cat(reset, loaded)},
		{name: "a masked loop starting anywhere", body: cat(param, loaded), checks: 1, entry: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &wasm.Module{
				TypeSection:     []wasm.FunctionType{{Params: []wasm.ValueType{wasm.ValueTypeI32}, ParamNumInUint64: 1}},
				MemorySection:   []wasm.Memory{{Min: 1, Max: 1}},
				FunctionSection: []wasm.Index{0},
				CodeSection:     []wasm.Code{{LocalTypes: []wasm.ValueType{wasm.ValueTypeI32}, Body: tc.body}},
			}
			text := lowerLast(t, m, api.CoreFeaturesV2)
			checks := strings.Count(text, "= PreemptRequested\n")
			_, _, entry := preemptChecks(t, m, api.CoreFeaturesV2)
			require.Equal(t, tc.checks, checks, text)
			require.Equal(t, tc.entry, entry, text)
		})
	}
}

// An indirect call checks first in a module that another module's function may
// reach it from (see wasm.Module.ForeignFuncRefs), and only there.
func TestPreemptCheckForeignCalls(t *testing.T) {
	for _, tc := range []struct {
		name    string
		exports []wasm.Export
		init    wasm.Index
		checked bool
	}{
		{name: "a private table of its own functions", init: 1},
		{name: "an exported table", init: 1, exports: []wasm.Export{{Name: "t", Type: wasm.ExternTypeTable}}, checked: true},
		{name: "an import in the table", init: 0, checked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &wasm.Module{
				TypeSection:         []wasm.FunctionType{{}},
				ImportSection:       []wasm.Import{{Module: "host", Name: "f", Type: wasm.ExternTypeFunc}},
				ImportFunctionCount: 1,
				TableSection:        []wasm.Table{{Min: 1, Type: wasm.RefTypeFuncref}},
				ElementSection: []wasm.ElementSegment{{
					OffsetExpr: wasm.NewConstantExpressionFromI32(0), Init: []wasm.Index{tc.init},
					Type: wasm.RefTypeFuncref, Mode: wasm.ElementModeActive,
				}},
				ExportSection:   tc.exports,
				FunctionSection: []wasm.Index{0, 0},
				CodeSection: []wasm.Code{
					{Body: []byte{wasm.OpcodeEnd}},
					{Body: []byte{wasm.OpcodeI32Const, 0, wasm.OpcodeCallIndirect, 0, 0, wasm.OpcodeEnd}},
				},
			}
			text := lowerLast(t, m, api.CoreFeaturesV2)
			require.Equal(t, tc.checked, m.ForeignFuncRefs)
			require.Equal(t, tc.checked, strings.Contains(text, "= PreemptRequested\n"), text)
		})
	}
}

// A bulk operation that may be long checks first, unless the function checks on
// entry; a loop around it keeps its full check either way, unless that check
// covers it.
func TestPreemptCheckBulk(t *testing.T) {
	fill := []byte{wasm.OpcodeI32Const, 0, wasm.OpcodeI32Const, 0, wasm.OpcodeLocalGet, 0, wasm.OpcodeMiscPrefix, wasm.OpcodeMiscMemoryFill, 0}
	loop := slices.Concat([]byte{wasm.OpcodeLoop, 0x40}, fill, []byte{wasm.OpcodeLocalGet, 0, wasm.OpcodeBrIf, 0, wasm.OpcodeEnd})
	opening := []byte{wasm.OpcodeI32Const, 0, wasm.OpcodeDrop} // so the loop does not stand in for the entry check
	recurse := []byte{wasm.OpcodeLocalGet, 0, wasm.OpcodeBrIf, 0, wasm.OpcodeLocalGet, 0, wasm.OpcodeCall, 0}
	for _, tc := range []struct {
		name   string
		body   [][]byte
		checks int // not elided, the entry block's included
	}{
		{name: "a fill", body: [][]byte{fill}, checks: 1},
		{name: "two fills", body: [][]byte{fill, fill}, checks: 2},
		{name: "two fills in a function checked on entry", body: [][]byte{fill, fill, recurse}, checks: 1},
		{name: "a fill in a loop", body: [][]byte{opening, loop}, checks: 1},
		{name: "a fill in a loop of a function checked on entry", body: [][]byte{opening, loop, recurse}, checks: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &wasm.Module{
				TypeSection:     []wasm.FunctionType{{Params: []wasm.ValueType{wasm.ValueTypeI32}, ParamNumInUint64: 1}},
				MemorySection:   []wasm.Memory{{Min: 1, Max: 1}},
				FunctionSection: []wasm.Index{0},
				CodeSection:     []wasm.Code{{Body: append(slices.Concat(tc.body...), wasm.OpcodeEnd)}},
			}
			text := lowerLast(t, m, api.CoreFeaturesV2)
			require.Equal(t, tc.checks, strings.Count(text, "= PreemptRequested\n"), text)
		})
	}
}

// A load's bounds check holds past a check made before a bulk operation, whose slow
// path only yields to Go: the same load after it is not checked again.
func TestPreemptCheckKeepsKnownBounds(t *testing.T) {
	load := []byte{wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Load, 2, 0, wasm.OpcodeDrop}
	body := append([]byte{}, load...)
	body = append(body, wasm.OpcodeLocalGet, 1, wasm.OpcodeLocalGet, 1, wasm.OpcodeLocalGet, 1,
		wasm.OpcodeMiscPrefix, wasm.OpcodeMiscMemoryCopy, 0, 0)
	body = append(append(body, load...), wasm.OpcodeEnd)
	m := &wasm.Module{
		TypeSection:     []wasm.FunctionType{{Params: []wasm.ValueType{wasm.ValueTypeI32, wasm.ValueTypeI32}, ParamNumInUint64: 2}},
		MemorySection:   []wasm.Memory{{Min: 1, Max: 1}},
		FunctionSection: []wasm.Index{0},
		CodeSection:     []wasm.Code{{Body: body}},
	}
	text := lowerLast(t, m, api.CoreFeaturesV2)
	require.Equal(t, 1, strings.Count(text, "= PreemptRequested\n"), text)
	// One for the first load, and one each for the copy's destination and source.
	require.Equal(t, 3, strings.Count(text, "memory_out_of_bounds"), text)
}

// A full loop check's slow path goes on into the body; a masked check's goes back
// to the header, whose check then runs again, since the backend may have moved the
// loop's exit test into the check's out-of-line half.
func TestPreemptCheckSlowPathTarget(t *testing.T) {
	for _, tc := range []struct {
		name, target string
		body         []byte
	}{
		{
			name: "full check", target: "blk4",
			body: []byte{wasm.OpcodeLoop, 0x40, wasm.OpcodeLocalGet, 0, wasm.OpcodeBrIf, 0, wasm.OpcodeEnd, wasm.OpcodeEnd},
		},
		{
			name: "masked check", target: "blk1",
			body: []byte{
				wasm.OpcodeLoop, 0x40,
				wasm.OpcodeLocalGet, 1, wasm.OpcodeI32Const, 1, wasm.OpcodeI32Add, wasm.OpcodeLocalTee, 1,
				wasm.OpcodeI32Load, 2, 0, wasm.OpcodeBrIf, 0,
				wasm.OpcodeEnd, wasm.OpcodeEnd,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &wasm.Module{
				TypeSection:     []wasm.FunctionType{{Params: []wasm.ValueType{wasm.ValueTypeI32}, ParamNumInUint64: 1}},
				MemorySection:   []wasm.Memory{{Min: 1, Max: 1}},
				FunctionSection: []wasm.Index{0},
				CodeSection:     []wasm.Code{{LocalTypes: []wasm.ValueType{wasm.ValueTypeI32}, Body: tc.body}},
			}
			text := lowerLast(t, m, api.CoreFeaturesV2)
			for _, blk := range strings.Split(strings.TrimSpace(text), "\n\n") {
				if strings.Contains(blk, "\tYield ") {
					require.True(t, strings.Contains(blk, "\tJump "+tc.target+"\n") || strings.HasSuffix(blk, "\tJump "+tc.target) ||
						strings.Contains(blk, "\tJump "+tc.target+","), text)
					return
				}
			}
			t.Fatalf("no slow path:\n%s", text)
		})
	}
}

// A loop whose body sets a local on the sides of ifs leaves its counter and its bound reaching their tests
// and the back edge through parameters of the joins the ifs make, which every edge passes one value: the
// loop is strip-mined as one without the ifs is, its check made once a chunk, not on every iteration.
func TestPreemptCheckCounterPastJoin(t *testing.T) {
	const i32 = wasm.ValueTypeI32
	body := []byte{
		wasm.OpcodeLoop, 0x40,
		wasm.OpcodeBlock, 0x40,
		wasm.OpcodeBlock, 0x40,
		wasm.OpcodeLocalGet, 1, wasm.OpcodeI32Const, 3, wasm.OpcodeI32And, wasm.OpcodeBrIf, 0,
		wasm.OpcodeI32Const, 1, wasm.OpcodeLocalSet, 2, wasm.OpcodeBr, 1,
		wasm.OpcodeEnd,
		wasm.OpcodeI32Const, 2, wasm.OpcodeLocalSet, 2,
		wasm.OpcodeEnd,
		wasm.OpcodeBlock, 0x40,
		wasm.OpcodeLocalGet, 2, wasm.OpcodeBrIf, 0,
		wasm.OpcodeI32Const, 5, wasm.OpcodeLocalSet, 2,
		wasm.OpcodeEnd,
		wasm.OpcodeLocalGet, 1, wasm.OpcodeI32Const, 1, wasm.OpcodeI32Add, wasm.OpcodeLocalTee, 1,
		wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Ne, wasm.OpcodeBrIf, 0,
		wasm.OpcodeEnd,
		wasm.OpcodeEnd,
	}
	m := &wasm.Module{
		TypeSection:     []wasm.FunctionType{{Params: []wasm.ValueType{i32}, ParamNumInUint64: 1}},
		FunctionSection: []wasm.Index{0},
		CodeSection:     []wasm.Code{{LocalTypes: []wasm.ValueType{i32, i32}, Body: body}},
	}
	// Strip-mined, the loop checks on the way in and where it refills its limit; with the check on
	// every iteration, it would make one at its header and none else.
	text := lowerLast(t, m, api.CoreFeaturesV2)
	require.Equal(t, 2, strings.Count(text, "= PreemptRequested\n"), text)
}
