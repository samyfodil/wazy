package component_test

import (
	"bytes"
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/component"
)

//go:embed testdata/adder.wasm
var adderWasm []byte

//go:embed testdata/hello.wasm
var helloWasm []byte

// TestInstantiate_AdderCall drives the public API end to end: instantiate a real
// component and call one of its exports through the exported wazy/component API.
func TestInstantiate_AdderCall(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := component.Instantiate(ctx, r, adderWasm)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	got, err := inst.CallExport(ctx, "component:adder/calc", "add", uint32(2), uint32(3))
	if err != nil {
		t.Fatalf("CallExport add: %v", err)
	}
	if len(got) != 1 || got[0].(uint32) != 5 {
		t.Fatalf("add(2,3) = %v, want [5]", got)
	}
}

// TestInstantiate_WASIHelloWorld drives WithWASI: a genuine rustc wasm32-wasip2
// wasi:cli/command that prints "hello world" through the wired WASI 0.2 stdio.
func TestInstantiate_WASIHelloWorld(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	var stdout, stderr bytes.Buffer
	inst, err := component.Instantiate(ctx, r, helloWasm,
		component.WithWASI(component.WASIConfig{Stdout: &stdout, Stderr: &stderr})...)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	if _, err := inst.Call(ctx, "wasi:cli/run@0.2.3#run"); err != nil {
		t.Fatalf("run: %v (stderr: %q)", err, stderr.String())
	}
	if got := stdout.String(); got != "hello world\n" {
		t.Fatalf("stdout = %q, want %q", got, "hello world\n")
	}
}

// TestCompileCache reuses one CompileCache across two instantiations of the same
// component, exercising the public caching entry points.
func TestCompileCache(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	cache := component.NewCompileCache()
	defer cache.Close(ctx)

	for i := range 2 {
		inst, err := component.Instantiate(ctx, r, adderWasm, component.WithCompileCache(cache))
		if err != nil {
			t.Fatalf("Instantiate #%d: %v", i, err)
		}
		got, err := inst.CallExport(ctx, "component:adder/calc", "add", uint32(10), uint32(20))
		if err != nil {
			t.Fatalf("add #%d: %v", i, err)
		}
		if got[0].(uint32) != 30 {
			t.Fatalf("add(10,20) = %v, want [30]", got)
		}
		if err := inst.Close(ctx); err != nil {
			t.Fatalf("Close #%d: %v", i, err)
		}
	}
}
