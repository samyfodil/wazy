package adhoc

import (
	"context"
	"encoding/binary"
	"runtime"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/leb128"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

// The values yieldRegisters keeps live: more integers and vectors than either
// backend has registers, so that every allocatable register holds one.
const (
	yieldInts      = 30
	yieldVecs      = 34
	yieldRecInts   = 6 // what rec passes on, as many as the ABI puts in registers
	yieldRecVecs   = 6
	yieldMul       = 0x5851f42d4c957f2d
	yieldLoopIters = 1 << 22
	yieldRecDepth  = 200
	yieldRecCalls  = 1 << 15
)

// TestYieldPreservesRegisters runs code with every register live across a
// preemption check that keeps yielding: a loop's check, and the entry check of a
// recursive function. A check yields through a trampoline that has to give every
// register back as it found it, so one it misses changes the result. Collections
// make the checks yield: each stops the world, which compiled code holds up until
// it yields, so one that completes while a single call runs proves the call
// yielded.
func TestYieldPreservesRegisters(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
	defer r.Close(ctx)
	mod, err := r.Instantiate(ctx, binaryencoding.EncodeModule(yieldRegistersModule()))
	require.NoError(t, err)

	for _, tc := range []struct {
		name string
		arg  uint64
		want uint64
	}{
		{"loop", yieldLoopIters, yieldLoopReference(yieldLoopIters)},
		{"recurse", yieldRecCalls, yieldRecurseReference(yieldRecCalls)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			callYielding(t, func() {
				got, err := mod.ExportedFunction(tc.name).Call(ctx, tc.arg)
				require.NoError(t, err)
				require.Equal(t, tc.want, got[0])
			})
		})
	}
}

// callYielding runs call while another goroutine forces collections, until one
// completes while call runs: each stops the world, which compiled code holds up
// until it yields, so that call yielded. A loaded machine can leave the
// collecting goroutine unscheduled for a whole call, so it tries a few times.
func callYielding(t *testing.T, call func()) {
	for attempt := 0; ; attempt++ {
		done, collections := make(chan struct{}), make(chan int)
		go func() {
			n := 0
			for {
				runtime.GC()
				select {
				case <-done:
					collections <- n
					return
				default:
					n++
				}
			}
		}()
		call()
		close(done)
		if n := <-collections; n > 0 {
			t.Logf("%d collections during the call", n)
			return
		}
		require.True(t, attempt < 20, "no collection completed during a call, so none yielded")
	}
}

// yieldRegistersModule builds, with i the i64 values and v the vectors:
//
//	(func $loop (param $n i32) (result i64)
//	  i[k] = k * yieldMul;  v[k] = i32x4.splat(k+1)
//	  loop: i[k] = i[k] * yieldMul + (k+1) for each k
//	        v[k] = v[k] + v[k+1 mod len] for each k, in order
//	        br_if loop (n = n - 1)
//	  the sum of every i[k] and every v[k]'s two i64 lanes)
//
//	(func $rec (param $d i32) (param i64 x yieldRecInts) (param v128 x yieldRecVecs) (result i64)
//	  d == 0 ? the same sum : $rec(d-1, i[k] * yieldMul + (k+1)..., v[k] + v[k+1 mod len]...))
//
//	(func $recurse (param $m i32) (result i64)
//	  the sum of $rec(yieldRecDepth, m+k..., i32x4.splat(m+k)...) while m counts down)
func yieldRegistersModule() *wasm.Module {
	i64c := func(v uint64) []byte { return append([]byte{wasm.OpcodeI64Const}, leb128s(int64(v))...) }
	i32c := func(v int32) []byte { return append([]byte{wasm.OpcodeI32Const}, leb128s(int64(v))...) }
	vec := func(op uint32) []byte { return append([]byte{wasm.OpcodeVecPrefix}, leb128.EncodeUint32(op)...) }
	get := func(l int) []byte { return append([]byte{wasm.OpcodeLocalGet}, leb128.EncodeUint32(uint32(l))...) }
	set := func(l int) []byte { return append([]byte{wasm.OpcodeLocalSet}, leb128.EncodeUint32(uint32(l))...) }
	cat := func(parts ...[]byte) (b []byte) {
		for _, p := range parts {
			b = append(b, p...)
		}
		return b
	}
	// sum folds ints integers at first and vecs vectors after them.
	sum := func(first, ints, vecs int) []byte {
		b := i64c(0)
		for k := range ints {
			b = cat(b, get(first+k), []byte{wasm.OpcodeI64Add})
		}
		for k := range vecs {
			for lane := byte(0); lane < 2; lane++ {
				b = cat(b, get(first+ints+k), vec(wasm.OpcodeVecI64x2ExtractLane), []byte{lane, wasm.OpcodeI64Add})
			}
		}
		return b
	}
	// stepInts and stepVecs are the update both loops make.
	stepInt := func(l, k int) []byte {
		return cat(get(l), i64c(yieldMul), []byte{wasm.OpcodeI64Mul}, i64c(uint64(k+1)), []byte{wasm.OpcodeI64Add})
	}
	stepVec := func(first, k, vecs int) []byte {
		return cat(get(first+k), get(first+(k+1)%vecs), vec(wasm.OpcodeVecI32x4Add))
	}

	// $loop: local 0 is n, then the ints, then the vectors.
	var loop []byte
	for k := range yieldInts {
		loop = cat(loop, i64c(uint64(k)*yieldMul), set(1+k))
	}
	for k := range yieldVecs {
		loop = cat(loop, i32c(int32(k+1)), vec(wasm.OpcodeVecI32x4Splat), set(1+yieldInts+k))
	}
	loop = append(loop, wasm.OpcodeLoop, 0x40)
	for k := range yieldInts {
		loop = cat(loop, stepInt(1+k, k), set(1+k))
	}
	for k := range yieldVecs {
		loop = cat(loop, stepVec(1+yieldInts, k, yieldVecs), set(1+yieldInts+k))
	}
	loop = cat(loop, get(0), i32c(1), []byte{wasm.OpcodeI32Sub, wasm.OpcodeLocalTee, 0, wasm.OpcodeBrIf, 0, wasm.OpcodeEnd})
	loop = cat(loop, sum(1, yieldInts, yieldVecs), []byte{wasm.OpcodeEnd})

	// $rec, function 1: param 0 is d, then the ints, then the vectors.
	const recIndex = 1
	rec := cat(get(0), []byte{wasm.OpcodeI32Eqz, wasm.OpcodeIf, byte(wasm.ValueTypeI64)}, sum(1, yieldRecInts, yieldRecVecs),
		[]byte{wasm.OpcodeElse}, get(0), i32c(1), []byte{wasm.OpcodeI32Sub})
	for k := range yieldRecInts {
		rec = cat(rec, stepInt(1+k, k))
	}
	for k := range yieldRecVecs {
		rec = cat(rec, stepVec(1+yieldRecInts, k, yieldRecVecs))
	}
	rec = cat(rec, []byte{wasm.OpcodeCall, recIndex, wasm.OpcodeEnd, wasm.OpcodeEnd})

	// $recurse: local 0 is m, local 1 the sum.
	recurse := []byte{wasm.OpcodeLoop, 0x40}
	recurse = cat(recurse, get(1), i32c(yieldRecDepth))
	for k := range yieldRecInts {
		recurse = cat(recurse, get(0), []byte{wasm.OpcodeI64ExtendI32U}, i64c(uint64(k)), []byte{wasm.OpcodeI64Add})
	}
	for k := range yieldRecVecs {
		recurse = cat(recurse, get(0), i32c(int32(k)), []byte{wasm.OpcodeI32Add}, vec(wasm.OpcodeVecI32x4Splat))
	}
	recurse = cat(recurse, []byte{wasm.OpcodeCall, recIndex, wasm.OpcodeI64Add}, set(1),
		get(0), i32c(1), []byte{wasm.OpcodeI32Sub, wasm.OpcodeLocalTee, 0, wasm.OpcodeBrIf, 0, wasm.OpcodeEnd},
		get(1), []byte{wasm.OpcodeEnd})

	repeat := func(t wasm.ValueType, n int) []wasm.ValueType {
		ts := make([]wasm.ValueType, n)
		for k := range ts {
			ts[k] = t
		}
		return ts
	}
	recParams := append(append([]wasm.ValueType{wasm.ValueTypeI32},
		repeat(wasm.ValueTypeI64, yieldRecInts)...), repeat(wasm.ValueTypeV128, yieldRecVecs)...)
	return &wasm.Module{
		TypeSection: []wasm.FunctionType{
			{Params: []wasm.ValueType{wasm.ValueTypeI32}, Results: []wasm.ValueType{wasm.ValueTypeI64}},
			{Params: recParams, Results: []wasm.ValueType{wasm.ValueTypeI64}},
		},
		FunctionSection: []wasm.Index{0, 1, 0},
		CodeSection: []wasm.Code{
			{LocalTypes: append(repeat(wasm.ValueTypeI64, yieldInts), repeat(wasm.ValueTypeV128, yieldVecs)...), Body: loop},
			{Body: rec},
			{LocalTypes: []wasm.ValueType{wasm.ValueTypeI64}, Body: recurse},
		},
		ExportSection: []wasm.Export{
			{Name: "loop", Type: wasm.ExternTypeFunc, Index: 0},
			{Name: "recurse", Type: wasm.ExternTypeFunc, Index: 2},
		},
	}
}

// yieldVec is a v128 as four i32 lanes.
type yieldVec [4]uint32

func (v yieldVec) add(w yieldVec) (r yieldVec) {
	for i := range r {
		r[i] = v[i] + w[i]
	}
	return r
}

// sum is what the wasm adds of v: its two i64 lanes.
func (v yieldVec) sum() uint64 {
	var b [16]byte
	for i, l := range v {
		binary.LittleEndian.PutUint32(b[4*i:], l)
	}
	return binary.LittleEndian.Uint64(b[:8]) + binary.LittleEndian.Uint64(b[8:])
}

// yieldStep is an iteration of $loop, which updates the vectors one after
// another, or, simultaneous, the arguments $rec passes on, all computed first.
func yieldStep(ints []uint64, vecs []yieldVec, simultaneous bool) {
	for k := range ints {
		ints[k] = ints[k]*yieldMul + uint64(k+1)
	}
	first := vecs[0]
	for k := range vecs {
		next := vecs[(k+1)%len(vecs)]
		if simultaneous && k == len(vecs)-1 {
			next = first
		}
		vecs[k] = vecs[k].add(next)
	}
}

func yieldSum(ints []uint64, vecs []yieldVec) (s uint64) {
	for _, i := range ints {
		s += i
	}
	for _, v := range vecs {
		s += v.sum()
	}
	return s
}

func yieldLoopReference(n uint32) uint64 {
	ints, vecs := make([]uint64, yieldInts), make([]yieldVec, yieldVecs)
	for k := range ints {
		ints[k] = uint64(k) * yieldMul
	}
	for k := range vecs {
		vecs[k] = yieldVec{uint32(k + 1), uint32(k + 1), uint32(k + 1), uint32(k + 1)}
	}
	for {
		yieldStep(ints, vecs, false)
		if n--; n == 0 {
			return yieldSum(ints, vecs)
		}
	}
}

func yieldRecurseReference(m uint32) (s uint64) {
	ints, vecs := make([]uint64, yieldRecInts), make([]yieldVec, yieldRecVecs)
	for {
		for k := range ints {
			ints[k] = uint64(m) + uint64(k)
		}
		for k := range vecs {
			l := m + uint32(k)
			vecs[k] = yieldVec{l, l, l, l}
		}
		for range yieldRecDepth {
			yieldStep(ints, vecs, true)
		}
		s += yieldSum(ints, vecs)
		if m--; m == 0 {
			return s
		}
	}
}

// TestYieldDoesNotAllocate runs a call long enough for the runtime to ask its
// goroutine to stop many times, with a context that can never be done, and
// checks that yielding allocates nothing.
func TestYieldDoesNotAllocate(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	// (func (param $n i32) loop call $leaf; br_if 0 (n = n - 1) end)
	body := []byte{
		wasm.OpcodeLoop, 0x40, wasm.OpcodeCall, 0,
		wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Const, 1, wasm.OpcodeI32Sub, wasm.OpcodeLocalTee, 0, wasm.OpcodeBrIf, 0,
		wasm.OpcodeEnd, wasm.OpcodeEnd,
	}
	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
	defer r.Close(ctx)
	mod, err := r.Instantiate(ctx, binaryencoding.EncodeModule(&wasm.Module{
		TypeSection:     []wasm.FunctionType{{}, {Params: []wasm.ValueType{wasm.ValueTypeI32}}},
		FunctionSection: []wasm.Index{0, 1},
		CodeSection:     []wasm.Code{{Body: []byte{wasm.OpcodeEnd}}, {Body: body}},
		ExportSection:   []wasm.Export{{Name: "f", Type: wasm.ExternTypeFunc, Index: 1}},
	}))
	require.NoError(t, err)
	f, stack := mod.ExportedFunction("f"), []uint64{0}
	allocs := testing.AllocsPerRun(2, func() {
		stack[0] = 200_000_000
		require.NoError(t, f.CallWithStack(ctx, stack))
	})
	require.Equal(t, float64(0), allocs)
}
