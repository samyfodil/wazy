package vswazero

import (
	"context"
	"testing"

	wasmtime "github.com/bytecodealliance/wasmtime-go/v34"
)

// TestCase3Parity guards BenchmarkCase3: it verifies all three runtimes produce
// the same result for the one export that returns a value (fibonacci) and that
// every export runs without error on each runtime, so the benchmark is timing
// equivalent work rather than a runtime that silently short-circuits.
//
// It also records the host-call fact that reframes one benchmark line: base64
// invokes the env.get_random_string import 100 times per call while every other
// export makes zero host calls. base64's number therefore measures host-call
// crossing overhead (where wazy's native in-process Go call beats wasmtime's cgo
// boundary), NOT base64 codegen -- so it must not be read as a compute result.
func TestCase3Parity(t *testing.T) {
	ctx := context.Background()

	rWazy := newCaseRuntimeWazy(t)
	defer rWazy.Close(ctx)
	mWazy, err := rWazy.Instantiate(ctx, caseWasm)
	if err != nil {
		t.Fatal(err)
	}
	rWzo := newCaseRuntimeWazero(t)
	defer rWzo.Close(ctx)
	mWzo, err := rWzo.Instantiate(ctx, caseWasm)
	if err != nil {
		t.Fatal(err)
	}
	store, inst := wtCaseInstance(t)

	const wantFib = 832040 // fibonacci(30)
	for _, e := range caseExports {
		wy, err := mWazy.ExportedFunction(e.name).Call(ctx, uint64(uint32(e.arg)))
		if err != nil {
			t.Errorf("wazy %s: %v", e.name, err)
		}
		wz, err := mWzo.ExportedFunction(e.name).Call(ctx, uint64(uint32(e.arg)))
		if err != nil {
			t.Errorf("wazero %s: %v", e.name, err)
		}
		wtres, err := inst.GetFunc(store, e.name).Call(store, e.arg)
		if err != nil {
			t.Errorf("wasmtime %s: %v", e.name, err)
		}
		if e.name == "fibonacci" {
			if len(wy) != 1 || wy[0] != wantFib {
				t.Errorf("wazy fibonacci = %v, want %d", wy, wantFib)
			}
			if len(wz) != 1 || wz[0] != wantFib {
				t.Errorf("wazero fibonacci = %v, want %d", wz, wantFib)
			}
			if r, ok := wtres.(int32); !ok || int(r) != wantFib {
				t.Errorf("wasmtime fibonacci = %v, want %d", wtres, wantFib)
			}
		}
	}

	// Confirm base64 is the host-call outlier (100 crossings) and the rest are
	// pure compute (0), so BenchmarkCase3/base64 is read as a host-call, not a
	// codegen, comparison.
	for _, e := range caseExports {
		engine := wasmtime.NewEngine()
		st := wasmtime.NewStore(engine)
		st.SetWasi(wasmtime.NewWasiConfig())
		lk := wasmtime.NewLinker(engine)
		if err := lk.DefineWasi(); err != nil {
			t.Fatal(err)
		}
		calls := 0
		if err := lk.Define(st, "env", "get_random_string", wasmtime.WrapFunc(st, func(a, b int32) { calls++ })); err != nil {
			t.Fatal(err)
		}
		m, err := wasmtime.NewModule(engine, caseWasm)
		if err != nil {
			t.Fatal(err)
		}
		in, err := lk.Instantiate(st, m)
		if err != nil {
			t.Fatal(err)
		}
		if s := in.GetFunc(st, "_start"); s != nil {
			_, _ = s.Call(st)
		}
		before := calls
		if _, err := in.GetFunc(st, e.name).Call(st, e.arg); err != nil {
			t.Fatal(err)
		}
		got := calls - before
		want := 0
		if e.name == "base64" {
			want = 100
		}
		if got != want {
			t.Errorf("%s host calls = %d, want %d (benchmark interpretation depends on this)", e.name, got, want)
		}
	}
}
