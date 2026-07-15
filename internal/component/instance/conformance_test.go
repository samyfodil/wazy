package instance

import (
	"bytes"
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/abi"
)

// This file implements a DIFFERENTIAL CONFORMANCE harness: a battery of
// real rustc wasm32-wasip2 wasi:cli/command components (testdata/
// conformance/*.component.wasm), each with a golden stdout
// (testdata/conformance/*.stdout.golden) captured ONCE from wasmtime (the
// reference implementation) via:
//
//	wasmtime run <component> [args...] [--env K=V ...] [--dir <tmp>::/]
//
// TestConformance runs every fixture on wazy through the exact same
// WithWASI(Args/Env/FS) inputs used to produce its golden, and asserts
// wazy's stdout is byte-for-byte IDENTICAL to wasmtime's. wasmtime itself
// is NOT required at test time -- only the committed golden files are
// read (via go:embed), so this test runs anywhere Go runs.
//
// Unlike real_hello_test.go/real_args_test.go/etc (each proving ONE
// specific WASI call path in isolation, with hand-picked assertions),
// this harness is deliberately generic and wasmtime-referential: the
// fixtures were chosen to span distinct ABI/WASI feature axes (numeric
// formatting, unicode strings, args, env, collections lowered as
// list<T>/tuple<T>, recursion/variant/option/result, file read, file
// write+read-back, wasi:cli/exit, and large/streamed output), and the
// pass/fail signal is "does wazy's output match the reference
// implementation's", not any hand-authored expectation.
//
// # Fixture manifest
//
// Each entry below documents, in one place, exactly what Rust source
// produced the fixture and what wasmtime invocation produced its golden --
// the SAME Args/Env/FS must be threaded through WithWASI so wazy sees
// identical inputs.
var conformanceFixtures = []conformanceFixture{
	{
		name: "f01_arith",
		desc: "loops summing i64, println! width/precision floats, hex/HEX/bin/oct + alternate-form formatting",
		wasm: f01ArithWasm, golden: f01ArithGolden,
	},
	{
		name: "f02_strings",
		desc: "split/join/replace/.chars().rev(), to_uppercase, unicode (héllo wörld, CJK, emoji) -- exercises utf8 lowering/lifting",
		wasm: f02StringsWasm, golden: f02StringsGolden,
	},
	{
		name: "f03_args",
		desc: "std::env::args(): argc, per-arg echo, integer sum, join -- exercises WASIConfig.Args -> wasi:cli/environment.get-arguments",
		wasm: f03ArgsWasm, golden: f03ArgsGolden,
		args: []string{"10", "20", "hello", "5"},
	},
	{
		name: "f04_env",
		desc: "std::env::var/vars(): specific + prefix-filtered lookups -- exercises WASIConfig.Env -> wasi:cli/environment.get-environment",
		wasm: f04EnvWasm, golden: f04EnvGolden,
		env: []string{"WAZY_NAME=wazy", "WAZY_COUNT=42", "PATH_LIKE=/usr/bin"},
	},
	{
		name: "f05_collections",
		desc: "Vec sort/dedup/retain/map, HashMap insert + key-sorted iteration -- exercises list<T> lowering at nontrivial size/shape",
		wasm: f05CollectionsWasm, golden: f05CollectionsGolden,
	},
	{
		name: "f06_recursion",
		desc: "recursive fib/factorial, enum match (area-by-shape), Result<_,String>, Option -- exercises deep call stacks + variant/option/result value flow",
		wasm: f06RecursionWasm, golden: f06RecursionGolden,
	},
	{
		name: "f07_fileread",
		desc: "std::fs::read_to_string + to_uppercase -- exercises wasi:filesystem/types read path (preopens, descriptor, input-stream) via WASIConfig.FS",
		wasm: f07FilereadWasm, golden: f07FilereadGolden,
		fs: map[string][]byte{"/data.txt": f07FilereadInput},
	},
	{
		name: "f08_filewrite",
		desc: "std::fs::write then read_to_string -- exercises wasi:filesystem/types write path (open-at create/truncate, write-via-stream) via WASIConfig.FS",
		wasm: f08FilewriteWasm, golden: f08FilewriteGolden,
		fs: map[string][]byte{},
	},
	{
		name: "f09_exit",
		desc: "println! then std::process::exit(7) -- exercises wasi:cli/exit.exit; wasmtime maps any nonzero guest exit to reference process exit code 1 (wasi:cli/exit's status is result<_,_>, a bare Ok/Err signal with no room for an arbitrary integer code, so 7 itself is not observable through this interface on either implementation)",
		wasm: f09ExitWasm, golden: f09ExitGolden,
		wantCallErr: true,
	},
	{
		name: "f10_bigout",
		desc: "1000 numbered println! lines -- exercises repeated/streamed output-stream.write calls at volume",
		wasm: f10BigoutWasm, golden: f10BigoutGolden,
	},
}

type conformanceFixture struct {
	name string
	desc string

	wasm   []byte
	golden string

	args []string
	env  []string
	fs   map[string][]byte

	// wantCallErr is true for fixtures whose guest terminates via
	// wasi:cli/exit with a nonzero status (f09_exit): wazy has no host
	// process for a successful OR failing exit() to terminate (see
	// wasi.go's exit doc), so run() surfaces as a Go error either way, but
	// stdout written before the exit call is still fully captured and must
	// still match the golden exactly.
	wantCallErr bool
}

// TestConformance is the differential conformance harness: for every
// fixture in conformanceFixtures, instantiate the component on wazy with
// WithWASI configured from the fixture's args/env/fs, call
// wasi:cli/run@0.2.3#run, and assert stdout byte-for-byte matches the
// golden captured from wasmtime. Every fixture is run (not stopped at the
// first failure) so a single t.Logf tally at the end reports the true
// pass/fail count across the whole battery.
func TestConformance(t *testing.T) {
	pass, fail := 0, 0
	for _, fx := range conformanceFixtures {
		fx := fx
		t.Run(fx.name, func(t *testing.T) {
			ctx := context.Background()
			r := wazy.NewRuntime(ctx)
			defer r.Close(ctx)

			var stdout, stderr bytes.Buffer
			inst, err := Instantiate(ctx, r, fx.wasm, WithWASI(WASIConfig{
				Stdout: &stdout,
				Stderr: &stderr,
				Args:   fx.args,
				Env:    fx.env,
				FS:     fx.fs,
			})...)
			if err != nil {
				t.Fatalf("Instantiate: %v", err)
			}
			defer inst.Close(ctx)

			results, callErr := inst.Call(ctx, "wasi:cli/run@0.2.3#run")

			if fx.wantCallErr {
				if callErr == nil {
					t.Errorf("Call run(): expected an error (guest calls wasi:cli/exit with a nonzero status), got nil; results=%v", results)
				} else {
					t.Logf("Call run() error (expected, exit path): %v", callErr)
				}
			} else if callErr != nil {
				t.Fatalf("Call run(): %v (stdout so far: %q, stderr so far: %q)", callErr, stdout.String(), stderr.String())
			} else {
				if len(results) != 1 {
					t.Fatalf("run() returned %d result(s), want 1", len(results))
				}
				rv, ok := results[0].(abi.ResultValue)
				if !ok {
					t.Fatalf("run() result: expected abi.ResultValue, got %T (%v)", results[0], results[0])
				}
				if rv.IsErr {
					t.Fatalf("run() returned Err, want Ok (stderr: %q)", stderr.String())
				}
			}

			got := stdout.String()
			if got == fx.golden {
				pass++
				t.Logf("PASS %s: stdout matches wasmtime golden exactly (%d bytes) -- %s", fx.name, len(got), fx.desc)
				return
			}
			fail++
			t.Errorf("FAIL %s: stdout does NOT match wasmtime golden -- %s\n--- wazy (%d bytes) ---\n%s\n--- wasmtime golden (%d bytes) ---\n%s",
				fx.name, fx.desc, len(got), got, len(fx.golden), fx.golden)
		})
	}
	t.Logf("conformance tally: %d/%d fixtures matched wasmtime byte-for-byte", pass, pass+fail)
}

//go:embed testdata/conformance/f01_arith.component.wasm
var f01ArithWasm []byte

//go:embed testdata/conformance/f01_arith.stdout.golden
var f01ArithGolden string

//go:embed testdata/conformance/f02_strings.component.wasm
var f02StringsWasm []byte

//go:embed testdata/conformance/f02_strings.stdout.golden
var f02StringsGolden string

//go:embed testdata/conformance/f03_args.component.wasm
var f03ArgsWasm []byte

//go:embed testdata/conformance/f03_args.stdout.golden
var f03ArgsGolden string

//go:embed testdata/conformance/f04_env.component.wasm
var f04EnvWasm []byte

//go:embed testdata/conformance/f04_env.stdout.golden
var f04EnvGolden string

//go:embed testdata/conformance/f05_collections.component.wasm
var f05CollectionsWasm []byte

//go:embed testdata/conformance/f05_collections.stdout.golden
var f05CollectionsGolden string

//go:embed testdata/conformance/f06_recursion.component.wasm
var f06RecursionWasm []byte

//go:embed testdata/conformance/f06_recursion.stdout.golden
var f06RecursionGolden string

//go:embed testdata/conformance/f07_fileread.component.wasm
var f07FilereadWasm []byte

//go:embed testdata/conformance/f07_fileread.stdout.golden
var f07FilereadGolden string

//go:embed testdata/conformance/f07_fileread.input.data.txt
var f07FilereadInput []byte

//go:embed testdata/conformance/f08_filewrite.component.wasm
var f08FilewriteWasm []byte

//go:embed testdata/conformance/f08_filewrite.stdout.golden
var f08FilewriteGolden string

//go:embed testdata/conformance/f09_exit.component.wasm
var f09ExitWasm []byte

//go:embed testdata/conformance/f09_exit.stdout.golden
var f09ExitGolden string

//go:embed testdata/conformance/f10_bigout.component.wasm
var f10BigoutWasm []byte

//go:embed testdata/conformance/f10_bigout.stdout.golden
var f10BigoutGolden string
