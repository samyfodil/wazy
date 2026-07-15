package instance

import (
	"bytes"
	"context"
	"testing"

	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// wasiHostFunc extracts the raw HostFunc WithWASI registered for iface/name,
// so each closure can be exercised directly -- white-box, same package --
// without needing a full component instantiation for every branch. See
// TestRealHello_PrintsHelloWorld (real_hello_test.go) for the end-to-end
// proof that these are wired correctly into a real guest; these tests cover
// the behavior (including error branches a "hello world" run never reaches)
// in isolation.
func wasiHostFunc(t *testing.T, cfg WASIConfig, iface, name string) HostFunc {
	t.Helper()
	c := newConfig(WithWASI(cfg))
	// Mirrors what a real Instantiate does right after newHandleTable()
	// (see runResourceHooks' doc): wasi_fs.go's filesystem host funcs
	// (get-directories, open-at, read-via-stream, ...) need a live
	// *handleTable to mint own<T> handles through, which a real
	// Instantiate always provides before any host func can run but this
	// helper -- extracting a HostFunc directly, without instantiating a
	// guest -- otherwise would not.
	runResourceHooks(c, newHandleTable())
	hi, ok := c.imports[importKey{iface: iface, name: name}]
	if !ok {
		t.Fatalf("WithWASI did not register %q %q", iface, name)
	}
	return hi.fn
}

func TestWASI_GetStdout(t *testing.T) {
	fn := wasiHostFunc(t, WASIConfig{}, wasiIfaceStdout, "get-stdout")
	results, err := fn(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].(uint32) != wasiStdoutRep {
		t.Fatalf("get-stdout: %#v, want [%d]", results, wasiStdoutRep)
	}
}

func TestWASI_GetStderr(t *testing.T) {
	fn := wasiHostFunc(t, WASIConfig{}, wasiIfaceStderr, "get-stderr")
	results, err := fn(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].(uint32) != wasiStderrRep {
		t.Fatalf("get-stderr: %#v, want [%d]", results, wasiStderrRep)
	}
}

func TestWASI_GetStdin(t *testing.T) {
	fn := wasiHostFunc(t, WASIConfig{}, wasiIfaceStdin, "get-stdin")
	results, err := fn(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("get-stdin: %#v", results)
	}
	if _, ok := results[0].(uint32); !ok {
		t.Fatalf("get-stdin: expected a uint32 rep, got %T", results[0])
	}
}

func TestWASI_Exit(t *testing.T) {
	fn := wasiHostFunc(t, WASIConfig{}, wasiIfaceExit, "exit")

	t.Run("ok", func(t *testing.T) {
		_, err := fn(context.Background(), []abi.Value{abi.ResultValue{IsErr: false}})
		requireErrContains(t, err, "wazy has no process to exit")
	})
	t.Run("err", func(t *testing.T) {
		_, err := fn(context.Background(), []abi.Value{abi.ResultValue{IsErr: true}})
		requireErrContains(t, err, "guest called exit with an error status")
	})
	t.Run("bad arg type", func(t *testing.T) {
		_, err := fn(context.Background(), []abi.Value{uint32(0)})
		requireErrContains(t, err, "expected a result")
	})
}

func TestWASI_GetEnvironment(t *testing.T) {
	fn := wasiHostFunc(t, WASIConfig{Env: []string{"A=1", "malformed", "B=two=deep"}}, wasiIfaceEnvironment, "get-environment")
	results, err := fn(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("get-environment: %#v", results)
	}
	pairs, ok := results[0].([]abi.Value)
	if !ok {
		t.Fatalf("get-environment: expected []abi.Value, got %T", results[0])
	}
	// "malformed" (no "=") is skipped; "B=two=deep" splits on the first "=".
	if len(pairs) != 2 {
		t.Fatalf("get-environment: got %d pair(s), want 2: %#v", len(pairs), pairs)
	}
	p0 := pairs[0].([]abi.Value)
	if p0[0].(string) != "A" || p0[1].(string) != "1" {
		t.Fatalf("pair[0] = %#v", p0)
	}
	p1 := pairs[1].([]abi.Value)
	if p1[0].(string) != "B" || p1[1].(string) != "two=deep" {
		t.Fatalf("pair[1] = %#v", p1)
	}
}

func TestWASI_GetEnvironment_Empty(t *testing.T) {
	fn := wasiHostFunc(t, WASIConfig{}, wasiIfaceEnvironment, "get-environment")
	results, err := fn(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	pairs := results[0].([]abi.Value)
	if len(pairs) != 0 {
		t.Fatalf("get-environment: got %d pair(s), want 0", len(pairs))
	}
}

func TestWASI_GetArguments(t *testing.T) {
	fn := wasiHostFunc(t, WASIConfig{Args: []string{"a", "b", "c"}}, wasiIfaceEnvironment, "get-arguments")
	results, err := fn(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("get-arguments: %#v", results)
	}
	args, ok := results[0].([]abi.Value)
	if !ok {
		t.Fatalf("get-arguments: expected []abi.Value, got %T", results[0])
	}
	// wasiArgv0 is prepended (see getArguments's doc) so a guest that skips
	// argv[0] (the Unix/WASI convention) sees exactly the configured Args.
	want := []string{wasiArgv0, "a", "b", "c"}
	if len(args) != len(want) {
		t.Fatalf("get-arguments: got %d arg(s), want %d: %#v", len(args), len(want), args)
	}
	for i, w := range want {
		if args[i].(string) != w {
			t.Fatalf("arg[%d] = %q, want %q", i, args[i], w)
		}
	}
}

func TestWASI_GetArguments_Empty(t *testing.T) {
	fn := wasiHostFunc(t, WASIConfig{}, wasiIfaceEnvironment, "get-arguments")
	results, err := fn(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	args := results[0].([]abi.Value)
	if len(args) != 1 || args[0].(string) != wasiArgv0 {
		t.Fatalf("get-arguments: got %#v, want just [%q] (wasiArgv0)", args, wasiArgv0)
	}
}

// TestWASI_GetDirectories proves get-directories returns exactly one
// preopened root descriptor ("/"), backed by a real, resolvable own<
// descriptor> handle -- see wasi_fs.go's package doc for why an empty
// result (this func's pre-filesystem behavior) makes a real guest's
// std::fs path fail before ever reaching a WASI call this package doesn't
// implement.
func TestWASI_GetDirectories(t *testing.T) {
	fn := wasiHostFunc(t, WASIConfig{}, wasiIfacePreopens, "get-directories")
	results, err := fn(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("get-directories: %#v", results)
	}
	dirs, ok := results[0].([]abi.Value)
	if !ok || len(dirs) != 1 {
		t.Fatalf("get-directories: got %#v, want a single-entry []abi.Value", results[0])
	}
	entry, ok := dirs[0].([]abi.Value)
	if !ok || len(entry) != 2 {
		t.Fatalf("get-directories[0]: got %#v, want a 2-element tuple<own<descriptor>,string>", dirs[0])
	}
	if _, ok := entry[0].(uint32); !ok {
		t.Fatalf("get-directories[0][0] (descriptor handle): got %#v (%T), want uint32", entry[0], entry[0])
	}
	if name, ok := entry[1].(string); !ok || name != "/" {
		t.Fatalf("get-directories[0][1] (name): got %#v, want \"/\"", entry[1])
	}
}

func TestWASI_CheckWrite(t *testing.T) {
	fn := wasiHostFunc(t, WASIConfig{}, wasiIfaceStreams, "[method]output-stream.check-write")

	t.Run("stdout", func(t *testing.T) {
		results, err := fn(context.Background(), []abi.Value{wasiStdoutRep})
		if err != nil {
			t.Fatal(err)
		}
		rv, ok := results[0].(abi.ResultValue)
		if !ok || rv.IsErr {
			t.Fatalf("check-write: %#v", results[0])
		}
		if rv.Payload.(uint64) == 0 {
			t.Fatal("check-write: expected a nonzero write budget")
		}
	})
	t.Run("unknown rep", func(t *testing.T) {
		_, err := fn(context.Background(), []abi.Value{uint32(99)})
		requireErrContains(t, err, "does not name a stdout/stderr stream")
	})
	t.Run("wrong arg count", func(t *testing.T) {
		_, err := fn(context.Background(), nil)
		requireErrContains(t, err, "expected 1 arg")
	})
	t.Run("wrong arg type", func(t *testing.T) {
		_, err := fn(context.Background(), []abi.Value{"x"})
		requireErrContains(t, err, "expected uint32 rep")
	})
}

func TestWASI_Write(t *testing.T) {
	var stdout, stderr bytes.Buffer
	fn := wasiHostFunc(t, WASIConfig{Stdout: &stdout, Stderr: &stderr}, wasiIfaceStreams, "[method]output-stream.write")

	t.Run("stdout", func(t *testing.T) {
		stdout.Reset()
		contents := []abi.Value{uint32('h'), uint32('i')}
		results, err := fn(context.Background(), []abi.Value{wasiStdoutRep, contents})
		if err != nil {
			t.Fatal(err)
		}
		rv, ok := results[0].(abi.ResultValue)
		if !ok || rv.IsErr {
			t.Fatalf("write: %#v", results[0])
		}
		if got := stdout.String(); got != "hi" {
			t.Fatalf("stdout = %q, want %q", got, "hi")
		}
	})
	t.Run("stderr", func(t *testing.T) {
		stderr.Reset()
		contents := []abi.Value{uint32('e'), uint32('!')}
		if _, err := fn(context.Background(), []abi.Value{wasiStderrRep, contents}); err != nil {
			t.Fatal(err)
		}
		if got := stderr.String(); got != "e!" {
			t.Fatalf("stderr = %q, want %q", got, "e!")
		}
	})
	t.Run("unknown rep", func(t *testing.T) {
		_, err := fn(context.Background(), []abi.Value{uint32(99), []abi.Value{}})
		requireErrContains(t, err, "does not name a stdout/stderr stream")
	})
	t.Run("wrong arg count", func(t *testing.T) {
		_, err := fn(context.Background(), []abi.Value{wasiStdoutRep})
		requireErrContains(t, err, "expected 2 args")
	})
	t.Run("wrong self type", func(t *testing.T) {
		_, err := fn(context.Background(), []abi.Value{"x", []abi.Value{}})
		requireErrContains(t, err, "expected uint32 rep")
	})
	t.Run("wrong contents type", func(t *testing.T) {
		_, err := fn(context.Background(), []abi.Value{wasiStdoutRep, "not a list"})
		requireErrContains(t, err, "expected list<u8>")
	})
	t.Run("contents element wrong type", func(t *testing.T) {
		_, err := fn(context.Background(), []abi.Value{wasiStdoutRep, []abi.Value{"x"}})
		requireErrContains(t, err, "expected uint32 (u8)")
	})
}

func TestWASI_BlockingWriteAndFlush_SharesWriteImpl(t *testing.T) {
	var stdout bytes.Buffer
	fn := wasiHostFunc(t, WASIConfig{Stdout: &stdout}, wasiIfaceStreams, "[method]output-stream.blocking-write-and-flush")
	contents := []abi.Value{uint32('o'), uint32('k')}
	results, err := fn(context.Background(), []abi.Value{wasiStdoutRep, contents})
	if err != nil {
		t.Fatal(err)
	}
	if rv := results[0].(abi.ResultValue); rv.IsErr {
		t.Fatalf("blocking-write-and-flush: %#v", rv)
	}
	if got := stdout.String(); got != "ok" {
		t.Fatalf("stdout = %q, want %q", got, "ok")
	}
}

func TestWASI_BlockingFlush(t *testing.T) {
	fn := wasiHostFunc(t, WASIConfig{}, wasiIfaceStreams, "[method]output-stream.blocking-flush")

	t.Run("stdout", func(t *testing.T) {
		results, err := fn(context.Background(), []abi.Value{wasiStdoutRep})
		if err != nil {
			t.Fatal(err)
		}
		if rv := results[0].(abi.ResultValue); rv.IsErr {
			t.Fatalf("blocking-flush: %#v", rv)
		}
	})
	t.Run("unknown rep", func(t *testing.T) {
		_, err := fn(context.Background(), []abi.Value{uint32(99)})
		requireErrContains(t, err, "does not name a stdout/stderr stream")
	})
	t.Run("wrong arg count", func(t *testing.T) {
		_, err := fn(context.Background(), nil)
		requireErrContains(t, err, "expected 1 arg")
	})
	t.Run("wrong arg type", func(t *testing.T) {
		_, err := fn(context.Background(), []abi.Value{"x"})
		requireErrContains(t, err, "expected uint32 rep")
	})
}

// TestWASI_NilWritersDiscard proves a zero-value WASIConfig (no Stdout/
// Stderr configured) doesn't panic on a write -- it discards, per
// WithWASI's doc.
func TestWASI_NilWritersDiscard(t *testing.T) {
	fn := wasiHostFunc(t, WASIConfig{}, wasiIfaceStreams, "[method]output-stream.write")
	contents := []abi.Value{uint32('x')}
	if _, err := fn(context.Background(), []abi.Value{wasiStdoutRep, contents}); err != nil {
		t.Fatal(err)
	}
}

func TestWasiBytesFromList(t *testing.T) {
	buf, err := wasiBytesFromList([]abi.Value{uint32('a'), uint32('b')})
	if err != nil {
		t.Fatal(err)
	}
	if string(buf) != "ab" {
		t.Fatalf("got %q, want %q", buf, "ab")
	}

	if _, err := wasiBytesFromList("not a list"); err == nil {
		t.Fatal("expected an error for a non-list value")
	}
	if _, err := wasiBytesFromList([]abi.Value{"x"}); err == nil {
		t.Fatal("expected an error for a non-uint32 element")
	}
}

func TestTypeTable(t *testing.T) {
	tbl := &typeTable{}

	primRef := tbl.add(binary.PrimitiveDesc{Prim: "u32"})
	if primRef.Primitive != "u32" || primRef.TypeIndex != nil {
		t.Fatalf("primitive add should not consume a table slot: %#v", primRef)
	}
	if len(tbl.entries) != 0 {
		t.Fatalf("table should still be empty after a primitive add, got %d entries", len(tbl.entries))
	}

	compRef := tbl.add(binary.ListDesc{Element: binary.TypeRef{Primitive: "u8"}})
	if compRef.TypeIndex == nil || *compRef.TypeIndex != 0 {
		t.Fatalf("composite add should return index 0, got %#v", compRef)
	}

	resolve := tbl.resolver()
	if _, ok := resolve(0).(binary.ListDesc); !ok {
		t.Fatalf("resolve(0) = %#v, want the ListDesc", resolve(0))
	}
	if resolve(1) != nil {
		t.Fatalf("resolve(1) = %#v, want nil (out of range)", resolve(1))
	}
}

// TestWasiFuncSigs sanity-checks the six hand-built FuncDesc/resolver pairs
// (see wasi.go's "Nested WIT types" doc): each one's params/results must
// flatten to real core types (proving the nested composite types --
// stream-error, list<tuple<string,string>>, list<tuple<own<descriptor>,
// string>> -- resolve correctly end to end), and the wide (>1 flat value)
// results must be the ones buildHostWrapper spills via an out-pointer.
func TestWasiFuncSigs(t *testing.T) {
	cases := []struct {
		name        string
		build       func() (binary.FuncDesc, abi.Resolver)
		wantParams  int
		wantResWide bool
	}{
		{"check-write", wasiCheckWriteSig, 1, true},
		{"write", wasiWriteSig, 2, true},
		{"blocking-flush", wasiBlockingFlushSig, 1, true},
		{"get-environment", wasiGetEnvironmentSig, 0, true},
		{"get-directories", wasiGetDirectoriesSig, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fd, resolve := tc.build()
			if len(fd.Params) != tc.wantParams {
				t.Fatalf("params: got %d, want %d", len(fd.Params), tc.wantParams)
			}
			_, coreResults, err := abi.FlattenFunc(fd, resolve, "lower")
			if err != nil {
				t.Fatalf("FlattenFunc: %v", err)
			}
			if tc.wantResWide {
				// "lower" spilling clears coreResults and appends one i32
				// out-pointer param -- see abi.FlattenFunc's doc.
				if len(coreResults) != 0 {
					t.Fatalf("coreResults: got %v, want empty (spilled)", coreResults)
				}
			}
		})
	}
}
