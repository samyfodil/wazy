package vswazero

import (
	"context"
	_ "embed"
)

// hostcallWasm is the guest module for BenchmarkHostCall. It is compiled from
// testdata/hostcall.wat (checked in) with wat2wasm and mirrors the hand-encoded
// module in internal/integration_test/bench/hostfunc_bench_test.go. The SAME
// bytes are fed to both runtimes so the comparison isolates runtime behaviour.
//
//go:embed testdata/hostcall.wasm
var hostcallWasm []byte

// caseWasm is a real, nontrivial TinyGo module copied verbatim from
// internal/integration_test/bench/testdata/case.wasm. It exports fibonacci,
// base64, string_manipulation, reverse_array and random_mat_mul, imports
// env.get_random_string and is initialised as a WASI command. The SAME bytes
// are compiled/instantiated/executed by both runtimes.
//
//go:embed testdata/case.wasm
var caseWasm []byte

// benchCtx is the context passed to every Call in the timed loops.
var benchCtx = context.Background()

// benchFn is the minimal subset of api.Function shared verbatim by
// github.com/samyfodil/wazy/api and github.com/tetratelabs/wazero/api. Both
// runtimes' concrete api.Function values satisfy it structurally, which lets a
// single benchmark/test body drive either runtime without importing either
// api package here (avoiding the two-packages-named-"api" collision).
type benchFn interface {
	Call(ctx context.Context, params ...uint64) ([]uint64, error)
	CallWithStack(ctx context.Context, stack []uint64) error
}

// hostCallEnv is the per-runtime fixture for BenchmarkHostCall: the two
// exported guest functions (keyed by host-registration mechanism), a helper to
// seed guest memory, and a cleanup func.
type hostCallEnv struct {
	// fns maps the host-registration mechanism to the guest function that
	// calls into it: "gomodule" -> call_go_host (WithGoModuleFunction),
	// "typed" -> call_go_typed_host (wazy HostFunc1 / wazero WithFunc).
	fns      map[string]benchFn
	writeMem func(offset, v uint32)
	close    func()
}

// Guest-module export names (see testdata/hostcall.wat).
const (
	callGoHostName      = "call_go_host"
	callGoTypedHostName = "call_go_typed_host"
)
