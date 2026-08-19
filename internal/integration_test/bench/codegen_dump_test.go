package bench

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
	"github.com/samyfodil/wazy/internal/platform"
)

// TestDumpCodegen compiles case.wasm so the backend prints what it generated.
//
// It is an inspection tool, not an assertion: machine code only appears when
// PrintFinalizedMachineCode is flipped to true in
// internal/engine/native/nativeapi/debug_options.go, since that is a build-time
// constant. With it off this compiles a module and checks nothing.
//
// Instruction and spill counts taken this way are deterministic and unaffected
// by whatever else the machine is doing, which makes them the honest way to
// judge a codegen change when the box is too loaded to time anything:
//
//	# in debug_options.go: PrintFinalizedMachineCode = true
//	go test -run TestDumpCodegen -v ./internal/integration_test/bench/ > /tmp/dump.txt
//	awk '/after finalize for .*random_mat_mul/{f=1}
//	     f&&/^\[\[\[after finalize/&&!/random_mat_mul/{f=0} f' /tmp/dump.txt > /tmp/fn.txt
//	wc -l < /tmp/fn.txt                       # instructions in that function
//	grep -cE '\(%rsp\)|\(%rbp\)' /tmp/fn.txt  # stack traffic, i.e. spills and reloads
//
// case.wasm is TinyGo output, so its exports (base64, string manipulation,
// reverse array, matmul) are representative of real producer code rather than
// hand-written wat.
func TestDumpCodegen(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	if !nativeapi.PrintFinalizedMachineCode {
		t.Log("PrintFinalizedMachineCode is off: compiling only, no machine code will be printed")
	}
	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
	defer r.Close(ctx)
	if _, err := r.CompileModule(ctx, caseWasm); err != nil {
		t.Fatal(err)
	}
}
