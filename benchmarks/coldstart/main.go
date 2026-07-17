// Command coldstart runs a wasip2 *command* component (one exporting
// wasi:cli/run) end to end -- decode, compile, instantiate, invoke run() --
// and exits. It exists to time wazy's component cold start against the
// `wasmtime run <component>` CLI on the identical component: both processes
// pay language-runtime startup + compile + instantiate + one call, measured as
// wall clock (see benchmarks/coldstart/run.sh). Not a library entry point;
// it imports internal/component/instance, so it lives inside the root module.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/instance"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: coldstart <component.wasm> [run-export]")
		os.Exit(2)
	}
	runExport := "wasi:cli/run@0.2.3#run"
	if len(os.Args) >= 3 {
		runExport = os.Args[2]
	}

	wasm, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "read:", err)
		os.Exit(1)
	}

	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := instance.Instantiate(ctx, r, wasm,
		instance.WithWASI(instance.WASIConfig{Stdout: os.Stdout, Stderr: os.Stderr})...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "instantiate:", err)
		os.Exit(1)
	}
	defer inst.Close(ctx)

	if _, err := inst.Call(ctx, runExport); err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		os.Exit(1)
	}
}
