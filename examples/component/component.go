package main

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"log"
	"strings"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/component"
)

// adder.wasm is a Component Model component exporting the
// `component:adder/calc` interface with an `add(u32, u32) -> u32` function.
//
//go:embed testdata/adder.wasm
var adderWasm []byte

// hello.wasm is a genuine rustc `wasm32-wasip2` `wasi:cli/command` that prints
// "hello world" through WASI 0.2 stdio.
//
//go:embed testdata/hello.wasm
var helloWasm []byte

// async_first_light.wasm exports `run-async`, a Component Model *async* export
// (callback ABI) that returns 42. From the embedder it is called exactly like a
// synchronous export — the async runtime is driven transparently underneath.
//
//go:embed testdata/async_first_light.wasm
var asyncWasm []byte

// main shows the three things wazy's component package does that upstream
// wazero does not: call a component interface export, run a WASI 0.2 command,
// and run a component that uses the async ABI.
func main() {
	ctx := context.Background()

	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx) // Closes everything this runtime created.

	fmt.Println(callInterfaceExport(ctx, r))
	fmt.Println(runWASICommand(ctx, r))
	fmt.Println(runAsyncExport(ctx, r))
}

// callInterfaceExport instantiates a component and calls one of its interface
// exports directly, passing and receiving lifted Component Model values.
func callInterfaceExport(ctx context.Context, r wazy.Runtime) string {
	inst, err := component.Instantiate(ctx, r, adderWasm)
	if err != nil {
		log.Panicf("instantiate adder: %v", err)
	}
	defer inst.Close(ctx)

	got, err := inst.CallExport(ctx, "component:adder/calc", "add", uint32(2), uint32(3))
	if err != nil {
		log.Panicf("call add: %v", err)
	}
	return fmt.Sprintf("component:adder/calc add(2, 3) = %d", got[0].(uint32))
}

// runWASICommand wires WASI 0.2 stdio and runs a wasi:cli/command component,
// capturing what it writes to stdout.
func runWASICommand(ctx context.Context, r wazy.Runtime) string {
	var stdout bytes.Buffer
	inst, err := component.Instantiate(ctx, r, helloWasm,
		component.WithWASI(component.WASIConfig{Stdout: &stdout})...)
	if err != nil {
		log.Panicf("instantiate hello: %v", err)
	}
	defer inst.Close(ctx)

	if _, err := inst.Call(ctx, "wasi:cli/run@0.2.3#run"); err != nil {
		log.Panicf("run hello: %v", err)
	}
	return "wasi:cli hello: " + strings.TrimSpace(stdout.String())
}

// runAsyncExport calls an async (callback-ABI) export. The call blocks until
// the async task completes; the Component Model async scheduler runs underneath.
func runAsyncExport(ctx context.Context, r wazy.Runtime) string {
	inst, err := component.Instantiate(ctx, r, asyncWasm)
	if err != nil {
		log.Panicf("instantiate async: %v", err)
	}
	defer inst.Close(ctx)

	got, err := inst.Call(ctx, "run-async")
	if err != nil {
		log.Panicf("call run-async: %v", err)
	}
	return fmt.Sprintf("async run-async() = %d", got[0].(uint32))
}
