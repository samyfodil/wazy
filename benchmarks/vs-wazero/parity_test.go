package vswazero

import (
	"math"
	"testing"
)

// fib is the reference implementation of the guest fibonacci export, used to
// assert both runtimes compute the expected value.
func fib(n uint64) uint64 {
	if n <= 1 {
		return n
	}
	return fib(n-1) + fib(n-2)
}

// TestHostCallParity asserts that, for identical guest bytes and inputs, wazy
// and wazero return byte-identical host-call results, for both host mechanisms
// and both call ops, and that the result equals the value seeded into memory.
func TestHostCallParity(t *testing.T) {
	wantBits := math.Float32bits(hostCallVal)

	envs := map[string]hostCallEnv{
		"wazy":   setupHostCallWazy(t),
		"wazero": setupHostCallWazero(t),
	}
	defer func() {
		for _, e := range envs {
			e.close()
		}
	}()
	for _, e := range envs {
		e.writeMem(hostCallOffset, wantBits)
	}

	for _, host := range []string{"gomodule", "typed"} {
		gotCall := map[string]uint32{}
		gotStack := map[string]uint32{}
		for name, e := range envs {
			res, err := e.fns[host].Call(benchCtx, uint64(hostCallOffset))
			if err != nil {
				t.Fatalf("%s/%s Call: %v", name, host, err)
			}
			gotCall[name] = uint32(res[0])

			stack := []uint64{uint64(hostCallOffset)}
			if err := e.fns[host].CallWithStack(benchCtx, stack); err != nil {
				t.Fatalf("%s/%s CallWithStack: %v", name, host, err)
			}
			gotStack[name] = uint32(stack[0])
		}
		if gotCall["wazy"] != wantBits || gotCall["wazero"] != wantBits {
			t.Errorf("host=%s Call: wazy=%#x wazero=%#x want=%#x", host, gotCall["wazy"], gotCall["wazero"], wantBits)
		}
		if gotStack["wazy"] != wantBits || gotStack["wazero"] != wantBits {
			t.Errorf("host=%s CallWithStack: wazy=%#x wazero=%#x want=%#x", host, gotStack["wazy"], gotStack["wazero"], wantBits)
		}
	}
}

// TestExecuteParity asserts that the fibonacci export returns identical, correct
// results under both runtimes.
func TestExecuteParity(t *testing.T) {
	wazyFn, wazyClose := setupExecuteWazy(t)
	defer wazyClose()
	wazeroFn, wazeroClose := setupExecuteWazero(t)
	defer wazeroClose()

	for _, n := range []uint64{5, 10, 20, 30} {
		want := fib(n)
		wr, err := wazyFn.Call(benchCtx, n)
		if err != nil {
			t.Fatalf("wazy fib(%d): %v", n, err)
		}
		zr, err := wazeroFn.Call(benchCtx, n)
		if err != nil {
			t.Fatalf("wazero fib(%d): %v", n, err)
		}
		if wr[0] != want || zr[0] != want {
			t.Errorf("fib(%d): wazy=%d wazero=%d want=%d", n, wr[0], zr[0], want)
		}
	}
}

// TestCompileInstantiateParity asserts caseWasm compiles and instantiates under
// both runtimes without error (the compile/instantiate benchmarks assume this).
func TestCompileInstantiateParity(t *testing.T) {
	for _, rt := range []struct {
		name    string
		compile func(*testing.T)
	}{
		{"wazy", func(t *testing.T) {
			r := newCaseRuntimeWazy(t)
			defer r.Close(benchCtx)
			c, err := r.CompileModule(benchCtx, caseWasm)
			if err != nil {
				t.Fatal(err)
			}
			mod, err := r.InstantiateModule(benchCtx, c, anonWazyConfig())
			if err != nil {
				t.Fatal(err)
			}
			mod.Close(benchCtx)
		}},
		{"wazero", func(t *testing.T) {
			r := newCaseRuntimeWazero(t)
			defer r.Close(benchCtx)
			c, err := r.CompileModule(benchCtx, caseWasm)
			if err != nil {
				t.Fatal(err)
			}
			mod, err := r.InstantiateModule(benchCtx, c, anonWazeroConfig())
			if err != nil {
				t.Fatal(err)
			}
			mod.Close(benchCtx)
		}},
	} {
		t.Run(rt.name, rt.compile)
	}
}
