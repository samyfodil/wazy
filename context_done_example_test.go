package wazy_test

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/samyfodil/wazy"
)

// infiniteLoopWasm exports a function named "infinite_loop" that never exits.
//
//go:embed internal/integration_test/engine/testdata/infinite_loop.wasm
var infiniteLoopWasm []byte

// Example_contextTimeout shows a context.WithTimeout ending a call that would
// otherwise never return. The call ends the way a trap does, and the module
// stays usable.
func Example_contextTimeout() {
	ctx := context.Background()

	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	moduleInstance, err := r.InstantiateWithConfig(ctx, infiniteLoopWasm,
		wazy.NewModuleConfig().WithName("malicious_wasm"))
	if err != nil {
		log.Panicln(err)
	}

	infiniteLoop := moduleInstance.ExportedFunction("infinite_loop")

	// Create the context.Context to be passed to the invocation of infinite_loop.
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()

	// Invoke the infinite loop with the timeout context.
	_, err = infiniteLoop.Call(ctx)

	// The error wraps the context's.
	fmt.Println(errors.Is(err, context.DeadlineExceeded))

	// Output:
	//	true
}

// Example_contextCancel shows a context.WithCancel ending a call that would
// otherwise never return.
func Example_contextCancel() {
	ctx := context.Background()

	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	moduleInstance, err := r.InstantiateWithConfig(ctx, infiniteLoopWasm,
		wazy.NewModuleConfig().WithName("malicious_wasm"))
	if err != nil {
		log.Panicln(err)
	}

	infiniteLoop := moduleInstance.ExportedFunction("infinite_loop")

	// Create the context.Context to be passed to the invocation of infinite_loop.
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		// After 2 seconds, cancel the invocation of infinite loop.
		time.Sleep(2 * time.Second)
		cancel()
	}()

	// Invoke the infinite loop with the cancelable context.
	_, err = infiniteLoop.Call(ctx)

	// The error wraps the context's.
	fmt.Println(errors.Is(err, context.Canceled))

	// Output:
	//	true
}

// Example_moduleClose shows closing a module, from another goroutine, ending a
// call into it that would otherwise never return.
func Example_moduleClose() {
	ctx := context.Background()

	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	moduleInstance, err := r.InstantiateWithConfig(ctx, infiniteLoopWasm,
		wazy.NewModuleConfig().WithName("malicious_wasm"))
	if err != nil {
		log.Panicln(err)
	}

	infiniteLoop := moduleInstance.ExportedFunction("infinite_loop")

	go func() {
		// After 2 seconds, close the module instance with CloseWithExitCode, which triggers the termination
		// of infinite loop.
		time.Sleep(2 * time.Second)
		if err := moduleInstance.CloseWithExitCode(ctx, 1); err != nil {
			log.Panicln(err)
		}
	}()

	// Invoke the infinite loop.
	_, err = infiniteLoop.Call(ctx)

	// The exit code is correctly handled and triggers the termination of infinite loop.
	fmt.Println(err)

	// Output:
	//	module closed with exit_code(1)
}
