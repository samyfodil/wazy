package instance

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// TestStdout_* is the M4 STEP 2 milestone proof: a component that imports
// two WIT interfaces -- test:cli/streams (a resource `output-stream` plus
// `[method]output-stream.write`) and test:cli/stdout (`get-stdout`) -- drives
// the exact shape a real WASI guest uses to print: get a stream handle, then
// write bytes to it. It exercises resources (M4.1's handle table),
// list<u8> marshalling, and multi-interface host imports (M3.3) together.
//
// See testdata/stdout_write.wat for the fixture. Its resource type is
// declared independently inside each of the two imports' instance types
// (rather than aliased from one to the other at the component level) --
// see the wat's comment for why: this package does not decode nested types
// inside an imported instance type, or type-sort aliases into the outer
// type index space (a pre-existing decoder gap; own/borrow signatures for
// host imports come from the caller via WithImport, not the binary). The
// two host funcs registered below are tied to the *same* host stream not by
// wasm-level type identity but by construction: both use the same
// outputStreamResourceType tag with the *Instance's shared resource handle
// table, and get-stdout always mints a handle for the one streamRep this
// test's host owns.
const outputStreamResourceType = 0

//go:embed testdata/stdout_write.wasm
var stdoutWriteWasm []byte

//go:embed testdata/stdout_write_alias.wasm
var stdoutWriteAliasWasm []byte

// stdoutStreamsImportOpts registers the two host imports the fixture needs:
// test:cli/stdout's get-stdout (mints an own<output-stream> handle naming
// streamRep) and test:cli/streams's [method]output-stream.write (resolves
// its borrow<output-stream> self argument back to a rep, requires it match
// streamRep, and appends the lifted list<u8> contents to out).
func stdoutStreamsImportOpts(out *bytes.Buffer, streamRep uint32) []Option {
	getStdout := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 0 {
			return nil, fmt.Errorf("get-stdout: expected 0 args, got %d", len(args))
		}
		// own<T>/borrow<T> results/args are host reps (uint32) at this layer;
		// allocHandleResult mints the guest-visible handle from it.
		return []abi.Value{streamRep}, nil
	}
	write := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 2 {
			return nil, fmt.Errorf("write: expected 2 args (self, contents), got %d", len(args))
		}
		rep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("write: self: expected uint32 rep, got %T", args[0])
		}
		if rep != streamRep {
			return nil, fmt.Errorf("write: self names rep %d, not the stream get-stdout returned (%d)", rep, streamRep)
		}
		contents, ok := args[1].([]abi.Value)
		if !ok {
			return nil, fmt.Errorf("write: contents: expected []abi.Value (list<u8>), got %T", args[1])
		}
		buf := make([]byte, len(contents))
		for i, v := range contents {
			b, ok := v.(uint32)
			if !ok {
				return nil, fmt.Errorf("write: contents[%d]: expected uint32 byte, got %T", i, v)
			}
			buf[i] = byte(b)
		}
		out.Write(buf)
		return nil, nil
	}
	return []Option{
		WithImport("test:cli/stdout", "get-stdout", getStdout,
			nil, []binary.TypeDesc{binary.OwnDesc{ResourceType: outputStreamResourceType}}),
		WithImport("test:cli/streams", "[method]output-stream.write", write,
			[]binary.TypeDesc{
				binary.BorrowDesc{ResourceType: outputStreamResourceType},
				binary.ListDesc{Element: binary.TypeRef{Primitive: "u8"}},
			}, nil),
	}
}

// instantiateStdout instantiates the stdout_write fixture with a fresh
// Runtime and a *bytes.Buffer as the injectable stdout target (the io.Writer
// the test asserts against), and registers t.Cleanup to close both.
func instantiateStdout(t *testing.T, streamRep uint32) (*Instance, *bytes.Buffer) {
	t.Helper()
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	t.Cleanup(func() { r.Close(ctx) })

	var out bytes.Buffer
	inst, err := Instantiate(ctx, r, stdoutWriteWasm, stdoutStreamsImportOpts(&out, streamRep)...)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	t.Cleanup(func() { inst.Close(ctx) })
	return inst, &out
}

// TestStdout_HelloWorld is the milestone proof itself: run() calls
// get-stdout then write(stream, "hello world" bytes); the host receives the
// borrow handle resolved back to the same stream get-stdout returned, lifts
// the list<u8> bytes out of the guest's memory, and appends them to a
// *bytes.Buffer the test owns and asserts against.
func TestStdout_HelloWorld(t *testing.T) {
	ctx := context.Background()
	inst, out := instantiateStdout(t, 1)

	if _, err := inst.Call(ctx, "run"); err != nil {
		t.Fatalf("Call run: %v", err)
	}
	if got := out.String(); got != "hello world" {
		t.Fatalf("captured %q, want %q", got, "hello world")
	}
}

// TestStdout_UnknownHandle drives write-handle with a handle value the guest
// never received from get-stdout, proving an unknown borrow<output-stream>
// handle fails loud through the real host-import call boundary (not just the
// resolveHandleArg unit tests in resource_test.go).
func TestStdout_UnknownHandle(t *testing.T) {
	ctx := context.Background()
	inst, out := instantiateStdout(t, 1)

	_, err := inst.Call(ctx, "write-handle", uint32(99999))
	if err == nil {
		t.Fatal("expected an error writing to an unknown stream handle")
	}
	requireErrContains(t, err, "unknown handle")
	if out.Len() != 0 {
		t.Fatalf("host buffer should not have been written to, got %q", out.String())
	}
}

// TestStdout_DroppedHandle proves a handle that named a real stream, but was
// already consumed (own's single-use semantics: liftHostArgs's TakeOwn
// removes the entry), fails loud on a later write rather than silently
// resolving to a stale rep.
func TestStdout_DroppedHandle(t *testing.T) {
	ctx := context.Background()
	inst, out := instantiateStdout(t, 1)

	results, err := inst.Call(ctx, "get-handle")
	if err != nil {
		t.Fatalf("Call get-handle: %v", err)
	}
	handle := results[0].(uint32)

	// Consume (drop) the handle directly against the instance's resource
	// table, exactly as an own<T> argument would be consumed by a real host
	// import (white-box: this test package is `instance`, so it can reach
	// the unexported field the same way the production lift/lower path
	// does).
	if _, err := inst.resources.TakeOwn(outputStreamResourceType, handle); err != nil {
		t.Fatalf("TakeOwn (simulating a drop): %v", err)
	}

	_, err = inst.Call(ctx, "write-handle", handle)
	if err == nil {
		t.Fatal("expected an error writing to a dropped stream handle")
	}
	requireErrContains(t, err, "unknown handle")
	if out.Len() != 0 {
		t.Fatalf("host buffer should not have been written to, got %q", out.String())
	}
}

// TestStdoutAlias_Decodes proves the decoder gap fixes (binary.Component's
// TypeSpace/ResolveType and AliasDef.CoreSort) by decoding
// stdout_write_alias.wasm -- the "natural" cross-import-alias pattern real
// WASI guests use (test:cli/stdout's get-stdout returns own<output-stream>
// of the SAME nominal resource aliased from test:cli/streams via `alias
// export $streams "output-stream" (type $ot_outer)`) -- and resolving every
// canon lift's TypeIdx through the full type index space. Before those
// fixes, the intervening type-sort alias shifted every later type-section
// deftype's true index past what binary.Component.Types (type-section-only)
// could see, and this would fail with "out of range of N types" instead of
// resolving to a func type.
func TestStdoutAlias_Decodes(t *testing.T) {
	comp, err := binary.Decode(bytes.NewReader(stdoutWriteAliasWasm))
	if err != nil {
		t.Fatalf("decode stdout_write_alias.wasm: %v", err)
	}

	for _, cn := range comp.Canons {
		if cn.Kind != 0x00 { // lift
			continue
		}
		td, err := comp.ResolveType(cn.TypeIdx)
		if err != nil {
			t.Fatalf("ResolveType(%d) for a canon lift: %v", cn.TypeIdx, err)
		}
		if _, ok := td.(binary.FuncDesc); !ok {
			t.Fatalf("ResolveType(%d) for a canon lift: got %T, want binary.FuncDesc", cn.TypeIdx, td)
		}
	}
}

// TestStdoutAlias_HelloWorld is the full milestone proof: the natural
// cross-import-alias fixture not only decodes (TestStdoutAlias_Decodes) but
// instantiates and runs exactly like stdout_write.wat's hand-worked-around
// version -- get-stdout then write(stream, "hello world" bytes) -- proving
// both decoder fixes together are sufficient to run a real-guest-shaped
// component end to end.
func TestStdoutAlias_HelloWorld(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	var out bytes.Buffer
	inst, err := Instantiate(ctx, r, stdoutWriteAliasWasm, stdoutStreamsImportOpts(&out, 1)...)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	if _, err := inst.Call(ctx, "run"); err != nil {
		t.Fatalf("Call run: %v", err)
	}
	if got := out.String(); got != "hello world" {
		t.Fatalf("captured %q, want %q", got, "hello world")
	}
}

// TestStdout_ListOutOfBounds proves an out-of-bounds list<u8> pointer (the
// fixture's write-oob export calls write with a valid handle but a
// ptr/len pair beyond the guest's memory) fails loud from LiftFlat rather
// than reading garbage or panicking.
func TestStdout_ListOutOfBounds(t *testing.T) {
	ctx := context.Background()
	inst, out := instantiateStdout(t, 1)

	handleResults, err := inst.Call(ctx, "get-handle")
	if err != nil {
		t.Fatalf("Call get-handle: %v", err)
	}
	handle := handleResults[0].(uint32)

	_, err = inst.Call(ctx, "write-oob", handle)
	if err == nil {
		t.Fatal("expected an error for an out-of-bounds list<u8> pointer")
	}
	requireErrContains(t, err, "overflow")
	if out.Len() != 0 {
		t.Fatalf("host buffer should not have been written to, got %q", out.String())
	}
}
