package adhoc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/engine/native/frontend"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
	"github.com/samyfodil/wazy/sys"
)

// loopYieldChildEnv tells a re-executed test binary which case to run: shape,
// GOMAXPROCS and whether the loop check is ablated, as "shape/procs/ablate".
const loopYieldChildEnv = "WAZY_LOOP_YIELD_CHILD"

// loopYieldLoops are endless guest loops, each without a call back into Go,
// so only the loop check can return them to Go.
var loopYieldLoops = map[string][]byte{
	// (loop br 0)
	"tight": {wasm.OpcodeLoop, 0x40, wasm.OpcodeBr, 0, wasm.OpcodeEnd, wasm.OpcodeEnd},
	// (loop local.get 0 i32.const 1 i32.add local.set 0 i32.const 1 br_if 0)
	"counted": {
		wasm.OpcodeLoop, 0x40,
		wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Const, 1, wasm.OpcodeI32Add, wasm.OpcodeLocalSet, 0,
		wasm.OpcodeI32Const, 1, wasm.OpcodeBrIf, 0,
		wasm.OpcodeEnd, wasm.OpcodeEnd,
	},
	// (loop call 1 br 0), where function 1 has no loop of its own.
	"call": {wasm.OpcodeLoop, 0x40, wasm.OpcodeCall, 1, wasm.OpcodeBr, 0, wasm.OpcodeEnd, wasm.OpcodeEnd},
	// "counted" with a call to function 1, which is short enough that the loop
	// keeps the check a loop with no call gets.
	"countedcall": {
		wasm.OpcodeLoop, 0x40, wasm.OpcodeCall, 1,
		wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Const, 1, wasm.OpcodeI32Add, wasm.OpcodeLocalSet, 0,
		wasm.OpcodeI32Const, 1, wasm.OpcodeBrIf, 0,
		wasm.OpcodeEnd, wasm.OpcodeEnd,
	},
	// A loop counting to 10 that always continues before it counts: one back
	// edge advances the counter, but the one taken passes it back unchanged.
	// (loop (br_if 0 (i32.const 1)) (local.tee 0 (i32.add (local.get 0) (i32.const 1)))
	//   (br_if 0 (i32.lt_u (i32.const 10))))
	"stuck": {
		wasm.OpcodeLoop, 0x40,
		wasm.OpcodeI32Const, 1, wasm.OpcodeBrIf, 0,
		wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Const, 1, wasm.OpcodeI32Add, wasm.OpcodeLocalTee, 0,
		wasm.OpcodeI32Const, 10, wasm.OpcodeI32LtU, wasm.OpcodeBrIf, 0,
		wasm.OpcodeEnd, wasm.OpcodeEnd,
	},
	// Stepping by two from 0xfffffffa while below 0xffffffff, which an even
	// counter never reaches: it wraps around past the bound, and on and on.
	"wrapping": {
		wasm.OpcodeI32Const, 0x7a, wasm.OpcodeLocalSet, 0,
		wasm.OpcodeLoop, 0x40,
		wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Const, 2, wasm.OpcodeI32Add, wasm.OpcodeLocalTee, 0,
		wasm.OpcodeI32Const, 0x7f, wasm.OpcodeI32LtU, wasm.OpcodeBrIf, 0,
		wasm.OpcodeEnd, wasm.OpcodeEnd,
	},
	// A loop that jumps on local 0, the way Go lays out a function, from its first
	// target forward to its second and from there back to the first:
	// (loop (block (block (br_table 0 1 (local.get 0))) (br 1 (local.set 0 (i32.const 1))))
	//   (br 0 (local.set 0 (i32.const 0))))
	"dispatch": {
		wasm.OpcodeLoop, 0x40,
		wasm.OpcodeBlock, 0x40, wasm.OpcodeBlock, 0x40,
		wasm.OpcodeLocalGet, 0, wasm.OpcodeBrTable, 1, 0, 1,
		wasm.OpcodeEnd,
		wasm.OpcodeI32Const, 1, wasm.OpcodeLocalSet, 0, wasm.OpcodeBr, 1,
		wasm.OpcodeEnd,
		wasm.OpcodeI32Const, 0, wasm.OpcodeLocalSet, 0, wasm.OpcodeBr, 0,
		wasm.OpcodeEnd, wasm.OpcodeEnd,
	},
	// An endless loop around a loop of a thousand iterations.
	"nested": {
		wasm.OpcodeLoop, 0x40,
		wasm.OpcodeI32Const, 0, wasm.OpcodeLocalSet, 0,
		wasm.OpcodeLoop, 0x40,
		wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Const, 1, wasm.OpcodeI32Add, wasm.OpcodeLocalTee, 0,
		wasm.OpcodeI32Const, 0xe8, 0x07, wasm.OpcodeI32LtU, wasm.OpcodeBrIf, 0,
		wasm.OpcodeEnd,
		wasm.OpcodeBr, 0,
		wasm.OpcodeEnd, wasm.OpcodeEnd,
	},
}

// loopYieldShapes are the cases TestLoopYield runs: each endless loop, and
// recursion that never passes a loop header, which only a check on function
// entry returns to Go. Each builds a module whose export "run" never returns, and
// names the features it needs.
var loopYieldShapes = func() map[string]func() (*wasm.Module, api.CoreFeatures) {
	shapes := map[string]func() (*wasm.Module, api.CoreFeatures){}
	for name, body := range loopYieldLoops {
		shapes[name] = func() (*wasm.Module, api.CoreFeatures) {
			return &wasm.Module{
				TypeSection:     []wasm.FunctionType{{}},
				FunctionSection: []wasm.Index{0, 0},
				CodeSection: []wasm.Code{
					{LocalTypes: []wasm.ValueType{wasm.ValueTypeI32}, Body: body},
					{Body: []byte{wasm.OpcodeEnd}},
				},
				ExportSection: []wasm.Export{{Name: "run", Type: wasm.ExternTypeFunc, Index: 0}},
			}, api.CoreFeaturesV2
		}
	}
	i32i32 := wasm.FunctionType{
		Params: []wasm.ValueType{wasm.ValueTypeI32}, Results: []wasm.ValueType{wasm.ValueTypeI32},
		ParamNumInUint64: 1, ResultNumInUint64: 1,
	}
	// A naive fib(60): no loop, a bounded depth, and 2^42 calls.
	shapes["calltree"] = func() (*wasm.Module, api.CoreFeatures) {
		return &wasm.Module{
			TypeSection:     []wasm.FunctionType{{}, i32i32},
			FunctionSection: []wasm.Index{0, 1},
			CodeSection: []wasm.Code{
				{Body: []byte{wasm.OpcodeI32Const, 60, wasm.OpcodeCall, 1, wasm.OpcodeDrop, wasm.OpcodeEnd}},
				{Body: []byte{
					wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Const, 2, wasm.OpcodeI32LtU,
					wasm.OpcodeIf, byte(wasm.ValueTypeI32),
					wasm.OpcodeLocalGet, 0,
					wasm.OpcodeElse,
					wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Const, 1, wasm.OpcodeI32Sub, wasm.OpcodeCall, 1,
					wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Const, 2, wasm.OpcodeI32Sub, wasm.OpcodeCall, 1,
					wasm.OpcodeI32Add,
					wasm.OpcodeEnd,
					wasm.OpcodeEnd,
				}},
			},
			ExportSection: []wasm.Export{{Name: "run", Type: wasm.ExternTypeFunc, Index: 0}},
		}, api.CoreFeaturesV2
	}
	// Forty functions each calling the next twice: no loop, no recursion, and 2^40
	// calls, which only checks on calls stop.
	shapes["callchain"] = func() (*wasm.Module, api.CoreFeatures) {
		const n = 41
		m := &wasm.Module{
			TypeSection:     []wasm.FunctionType{{}},
			FunctionSection: make([]wasm.Index, n),
			CodeSection:     make([]wasm.Code, n),
			ExportSection:   []wasm.Export{{Name: "run", Type: wasm.ExternTypeFunc, Index: 0}},
		}
		for i := range n - 1 {
			m.CodeSection[i].Body = []byte{wasm.OpcodeCall, byte(i + 1), wasm.OpcodeCall, byte(i + 1), wasm.OpcodeEnd}
		}
		m.CodeSection[n-1].Body = []byte{wasm.OpcodeEnd}
		return m, api.CoreFeaturesV2
	}
	// (func $run return_call $run): endless, at a fixed depth.
	shapes["tailcall"] = func() (*wasm.Module, api.CoreFeatures) {
		return &wasm.Module{
			TypeSection:     []wasm.FunctionType{{}},
			FunctionSection: []wasm.Index{0},
			CodeSection:     []wasm.Code{{Body: []byte{wasm.OpcodeTailCallReturnCall, 0, wasm.OpcodeEnd}}},
			ExportSection:   []wasm.Export{{Name: "run", Type: wasm.ExternTypeFunc, Index: 0}},
		}, api.CoreFeaturesV3
	}
	// (loop call $dep.g br 0), where dep.g is another module's empty function:
	// the call runs compiled code, not Go.
	shapes["import"] = func() (*wasm.Module, api.CoreFeatures) {
		return &wasm.Module{
			TypeSection:     []wasm.FunctionType{{}},
			ImportSection:   []wasm.Import{{Module: "dep", Name: "g", Type: wasm.ExternTypeFunc, DescFunc: 0}},
			FunctionSection: []wasm.Index{0},
			CodeSection: []wasm.Code{{Body: []byte{
				wasm.OpcodeLoop, 0x40, wasm.OpcodeCall, 0, wasm.OpcodeBr, 0, wasm.OpcodeEnd, wasm.OpcodeEnd,
			}}},
			ExportSection: []wasm.Export{{Name: "run", Type: wasm.ExternTypeFunc, Index: 1}},
		}, api.CoreFeaturesV2
	}
	// $run return_calls dep.g, which return_call_indirects through dep's
	// exported table, where run's module put $run: a cycle through two modules,
	// neither of which has one of its own by its direct calls. dep's analysis
	// closes it: g is exported, so its reference is taken, and its indirect call
	// may reach any such function. See wasm.entryCheckBuilder.
	shapes["crossmodule"] = func() (*wasm.Module, api.CoreFeatures) {
		return &wasm.Module{
			TypeSection: []wasm.FunctionType{{}},
			ImportSection: []wasm.Import{
				{Module: "dep", Name: "g", Type: wasm.ExternTypeFunc, DescFunc: 0},
				{Module: "dep", Name: "t", Type: wasm.ExternTypeTable, DescTable: wasm.Table{Min: 1, Type: wasm.RefTypeFuncref}},
			},
			FunctionSection: []wasm.Index{0},
			CodeSection:     []wasm.Code{{Body: []byte{wasm.OpcodeTailCallReturnCall, 0, wasm.OpcodeEnd}}},
			ElementSection: []wasm.ElementSegment{{
				OffsetExpr: wasm.NewConstantExpressionFromI32(0), TableIndex: 0, Init: []wasm.Index{1},
				Type: wasm.RefTypeFuncref, Mode: wasm.ElementModeActive,
			}},
			ExportSection: []wasm.Export{{Name: "run", Type: wasm.ExternTypeFunc, Index: 1}},
		}, api.CoreFeaturesV3
	}
	// Two functions that return_call each other.
	shapes["mutual"] = func() (*wasm.Module, api.CoreFeatures) {
		return &wasm.Module{
			TypeSection:     []wasm.FunctionType{{}},
			FunctionSection: []wasm.Index{0, 0},
			CodeSection: []wasm.Code{
				{Body: []byte{wasm.OpcodeTailCallReturnCall, 1, wasm.OpcodeEnd}},
				{Body: []byte{wasm.OpcodeTailCallReturnCall, 0, wasm.OpcodeEnd}},
			},
			ExportSection: []wasm.Export{{Name: "run", Type: wasm.ExternTypeFunc, Index: 0}},
		}, api.CoreFeaturesV3
	}
	return shapes
}()

// loopYieldDeps are the modules some shapes import from, instantiated first as
// "dep": an empty function g, and a one-entry table t that g return_call_indirects
// through.
var loopYieldDeps = map[string]func() *wasm.Module{
	"import": func() *wasm.Module {
		return &wasm.Module{
			TypeSection:     []wasm.FunctionType{{}},
			FunctionSection: []wasm.Index{0},
			CodeSection:     []wasm.Code{{Body: []byte{wasm.OpcodeEnd}}},
			ExportSection:   []wasm.Export{{Name: "g", Type: wasm.ExternTypeFunc, Index: 0}},
		}
	},
	"crossmodule": func() *wasm.Module {
		return &wasm.Module{
			TypeSection:     []wasm.FunctionType{{}},
			FunctionSection: []wasm.Index{0},
			TableSection:    []wasm.Table{{Min: 1, Type: wasm.RefTypeFuncref}},
			CodeSection: []wasm.Code{{Body: []byte{
				wasm.OpcodeI32Const, 0, wasm.OpcodeTailCallReturnCallIndirect, 0, 0, wasm.OpcodeEnd,
			}}},
			ExportSection: []wasm.Export{
				{Name: "g", Type: wasm.ExternTypeFunc, Index: 0},
				{Name: "t", Type: wasm.ExternTypeTable, Index: 0},
			},
		}
	},
}

// TestLoopYield runs each shape in a child process and, while it spins,
// has another goroutine force garbage collections and then close the module. A
// collection stops the world, which a compiled loop holds up until it returns
// to Go. With the loop check the child finishes; without it, it must hang, or
// the test would pass whether or not the check works. A hang freezes the test
// binary's own timeout with everything else, so the parent kills the child.
func TestLoopYield(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	if c := os.Getenv(loopYieldChildEnv); c != "" {
		loopYieldChild(t, c)
		return
	}
	for shape := range loopYieldShapes {
		for _, procs := range []int{1, 2} {
			for _, ablate := range []bool{false, true} {
				if ablate && testing.Short() {
					continue
				}
				c := fmt.Sprintf("%s/%d/%t", shape, procs, ablate)
				t.Run(c, func(t *testing.T) {
					t.Parallel()
					limit := time.Minute
					if ablate {
						limit = 3 * time.Second
					}
					ctx, cancel := context.WithTimeout(context.Background(), limit)
					defer cancel()
					cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLoopYield$", "-test.v")
					cmd.Env = append(os.Environ(), loopYieldChildEnv+"="+c)
					out, err := cmd.CombinedOutput()
					hung := ctx.Err() != nil
					switch {
					case ablate && !hung:
						t.Fatalf("without the loop check the child finished, so this shape proves nothing: %v\n%s", err, out)
					case !ablate && hung:
						t.Fatalf("the child hung for %v", limit)
					case !ablate && (err != nil || !strings.Contains(string(out), "loop yield: closed")):
						t.Fatalf("child failed: %v\n%s", err, out)
					}
				})
			}
		}
	}
}

func loopYieldChild(t *testing.T, c string) {
	var shape string
	var procs int
	var ablate bool
	if _, err := fmt.Sscanf(strings.ReplaceAll(c, "/", " "), "%s %d %t", &shape, &procs, &ablate); err != nil {
		t.Fatal(err)
	}
	runtime.GOMAXPROCS(procs)
	frontend.LoopYield = !ablate

	m, features := loopYieldShapes[shape]()
	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler().WithCoreFeatures(features))
	defer r.Close(ctx)
	if dep := loopYieldDeps[shape]; dep != nil {
		if _, err := r.InstantiateWithConfig(ctx, binaryencoding.EncodeModule(dep()), wazy.NewModuleConfig().WithName("dep")); err != nil {
			t.Fatal(err)
		}
	}
	mod, err := r.Instantiate(ctx, binaryencoding.EncodeModule(m))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := mod.ExportedFunction("run").Call(ctx)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	for range 5 {
		runtime.GC()
	}
	if err = mod.Close(ctx); err != nil {
		t.Fatal(err)
	}
	var exitErr *sys.ExitError
	if err = <-done; !errors.As(err, &exitErr) {
		t.Fatalf("want the error closing the module gives, got %v", err)
	}
	t.Log("loop yield: closed:", err)
}

// TestLoopYield_counted checks that a loop whose preemption check tests its
// counter first computes what the interpreter does, at bounds on both sides of
// the iterations the full check runs on. The full check runs out of line, so a
// mistake there sends the loop somewhere else entirely.
func TestLoopYield_counted(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	// (func (param n i32) (result i32) (local i s)
	//   (loop s += i; i += 1; br_if (i <u n)) s)
	mod := binaryencoding.EncodeModule(&wasm.Module{
		TypeSection: []wasm.FunctionType{{
			Params: []wasm.ValueType{wasm.ValueTypeI32}, Results: []wasm.ValueType{wasm.ValueTypeI32},
			ParamNumInUint64: 1, ResultNumInUint64: 1,
		}},
		FunctionSection: []wasm.Index{0},
		CodeSection: []wasm.Code{{LocalTypes: []wasm.ValueType{wasm.ValueTypeI32, wasm.ValueTypeI32}, Body: []byte{
			wasm.OpcodeLoop, 0x40,
			wasm.OpcodeLocalGet, 2, wasm.OpcodeLocalGet, 1, wasm.OpcodeI32Add, wasm.OpcodeLocalSet, 2,
			wasm.OpcodeLocalGet, 1, wasm.OpcodeI32Const, 1, wasm.OpcodeI32Add, wasm.OpcodeLocalTee, 1,
			wasm.OpcodeLocalGet, 0, wasm.OpcodeI32LtU, wasm.OpcodeBrIf, 0,
			wasm.OpcodeEnd,
			wasm.OpcodeLocalGet, 2,
			wasm.OpcodeEnd,
		}}},
		ExportSection: []wasm.Export{{Name: "sum", Type: wasm.ExternTypeFunc, Index: 0}},
	})
	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
	defer r.Close(ctx)
	m, err := r.Instantiate(ctx, mod)
	require.NoError(t, err)
	for _, n := range []uint32{1, 2, 4095, 4096, 4097, 100_000} {
		res, err := m.ExportedFunction("sum").Call(ctx, uint64(n))
		require.NoError(t, err)
		require.Equal(t, uint64(n*(n-1)/2), res[0])
	}
}

// TestLoopYield_preemptedAtExit runs a loop that counts down to zero and tests for
// zero first, whose exit test the backend folds into the out-of-line half of its
// masked check, while collections keep asking the goroutine to stop, so that some
// of its yields land on the way out. The slow path has to go back through that
// test: a loop that went on past zero would load outside the memory.
func TestLoopYield_preemptedAtExit(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	// (func (param i i32) (result i32) (local s i32)
	//   (block (loop (br_if 1 (i32.eqz i)) s += load8_u(i); i -= 1; br 0)) s)
	mod := binaryencoding.EncodeModule(&wasm.Module{
		TypeSection: []wasm.FunctionType{{
			Params: []wasm.ValueType{wasm.ValueTypeI32}, Results: []wasm.ValueType{wasm.ValueTypeI32},
			ParamNumInUint64: 1, ResultNumInUint64: 1,
		}},
		MemorySection:   []wasm.Memory{{Min: 1, Cap: 1, Max: 1, IsMaxEncoded: true}},
		FunctionSection: []wasm.Index{0},
		CodeSection: []wasm.Code{{LocalTypes: []wasm.ValueType{wasm.ValueTypeI32}, Body: []byte{
			wasm.OpcodeBlock, 0x40,
			wasm.OpcodeLoop, 0x40,
			wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Eqz, wasm.OpcodeBrIf, 1,
			wasm.OpcodeLocalGet, 1, wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Load8U, 0, 0, wasm.OpcodeI32Add, wasm.OpcodeLocalSet, 1,
			wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Const, 1, wasm.OpcodeI32Sub, wasm.OpcodeLocalSet, 0,
			wasm.OpcodeBr, 0,
			wasm.OpcodeEnd, wasm.OpcodeEnd,
			wasm.OpcodeLocalGet, 1,
			wasm.OpcodeEnd,
		}}},
		ExportSection: []wasm.Export{{Name: "count", Type: wasm.ExternTypeFunc, Index: 0}},
	})
	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
	defer r.Close(ctx)
	m, err := r.Instantiate(ctx, mod)
	require.NoError(t, err)
	count := m.ExportedFunction("count")

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				runtime.GC()
			}
		}
	}()
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		res, err := count.Call(ctx, 60000)
		require.NoError(t, err)
		require.Equal(t, uint64(0), res[0])
	}
}
