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

// thread.wasm (source: testdata/thread.wat) spawns a Component Model *thread*
// with `thread.new-indirect` and hands control to it with
// `thread.yield-then-resume`; the worker thread resolves the task with
// `task.return(99)`.
//
//go:embed testdata/thread.wasm
var threadWasm []byte

// await_import.wasm exports `run-async`, which awaits an async import
// `get() -> u32`. It is the component driven by the CallAsync demo: the host
// completes `get` from another goroutine.
//
//go:embed testdata/await_import.wasm
var awaitImportWasm []byte

// multithread.wasm (source: testdata/multithread.wat) spawns FIVE Component
// Model threads over one shared array: four mappers double their own chunk and
// a reducer sums the result. Map-reduce, 2*(1+..+8) = 72.
//
//go:embed testdata/multithread.wasm
var multiThreadWasm []byte

// main shows the things wazy's component package does that wazero does not: call
// a component interface export, run a WASI 0.2 command, run the async ABI, spawn
// a Component Model thread, and drive a component with CallAsync whose import
// completes on another goroutine.
func main() {
	ctx := context.Background()

	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx) // Closes everything this runtime created.

	fmt.Println(callInterfaceExport(ctx, r))
	fmt.Println(runWASICommand(ctx, r))
	fmt.Println(runAsyncExport(ctx, r))
	fmt.Println(runThreadExport(ctx, r))
	fmt.Println(runMultiThread(ctx, r))
	fmt.Println(runCallAsync(ctx, r))
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

// runThreadExport calls an export that spawns a Component Model thread
// (thread.new-indirect + thread.yield-then-resume). The worker thread resolves
// the task; from the embedder it is still one blocking Call.
func runThreadExport(ctx context.Context, r wazy.Runtime) string {
	inst, err := component.Instantiate(ctx, r, threadWasm)
	if err != nil {
		log.Panicf("instantiate thread: %v", err)
	}
	defer inst.Close(ctx)

	got, err := inst.Call(ctx, "run-async")
	if err != nil {
		log.Panicf("call thread run-async: %v", err)
	}
	return fmt.Sprintf("thread (spawn + resume) = %d", got[0].(uint32))
}

// runMultiThread spawns five Component Model threads over one shared array: four
// mappers each double their 2-element chunk, then a reducer sums the whole
// (doubled) array — a cooperative map-reduce entirely inside the guest.
func runMultiThread(ctx context.Context, r wazy.Runtime) string {
	inst, err := component.Instantiate(ctx, r, multiThreadWasm)
	if err != nil {
		log.Panicf("instantiate multithread: %v", err)
	}
	defer inst.Close(ctx)

	got, err := inst.Call(ctx, "run-async")
	if err != nil {
		log.Panicf("call multithread run-async: %v", err)
	}
	return fmt.Sprintf("multithread map-reduce (4 mappers + reducer) = %d", got[0].(uint32))
}

// runCallAsync drives a component with CallAsync: the guest awaits an async
// import that completes from ANOTHER goroutine (real I/O in a real host), after
// the import call returned — the flow a blocking Call cannot express. CallAsync
// returns a PendingCall the moment the guest parks; Await resumes it once the
// external Resolve lands.
func runCallAsync(ctx context.Context, r wazy.Runtime) string {
	release := make(chan struct{})
	// The "get() -> u32" import defers its result to a goroutine.
	getImport := component.WithAsyncImport("get", "",
		func(_ context.Context, _ []component.Value, call *component.AsyncCall) error {
			go func() {
				<-release // stand-in for I/O finishing on some other goroutine
				call.Resolve([]component.Value{uint32(42)})
			}()
			return nil
		},
		nil, // no params
		[]component.TypeDesc{component.PrimitiveDesc{Prim: "u32"}}, // -> u32
	)

	inst, err := component.Instantiate(ctx, r, awaitImportWasm, getImport)
	if err != nil {
		log.Panicf("instantiate await_import: %v", err)
	}
	defer inst.Close(ctx)

	p, err := inst.CallAsync(ctx, "run-async")
	if err != nil {
		log.Panicf("CallAsync: %v", err)
	}
	// The call is parked here, awaiting the external import — the host is free.
	pending := ""
	select {
	case <-p.Done():
	default:
		pending = " (was parked until the external goroutine resolved)"
	}

	close(release) // let the other goroutine complete the import
	got, err := p.Await(ctx)
	if err != nil {
		log.Panicf("Await: %v", err)
	}
	return fmt.Sprintf("callasync run-async() = %d%s", got[0].(uint32), pending)
}
