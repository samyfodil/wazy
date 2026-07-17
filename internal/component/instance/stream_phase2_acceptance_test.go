package instance

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// Phase 2 acceptance fixtures
// (docs/component-model-async-phase2-design.md's acceptance list). Both are
// deliberately SYNCHRONOUS guest exports/imports: the host arranges its side
// of the rendezvous (StreamReader.Read / FutureWriter.Set) before the guest
// runs, so the copy machinery's rendezvous completes INLINE inside the
// guest's stream.write/future.read builtin call -- no callback/async lift
// is needed to exercise the full stream/future runtime end to end. See
// testdata/async/stream_write_host_read.wat and
// testdata/async/future_write_host_write.wat for the component sources
// (built via `wasm-tools parse foo.wat -o foo.wasm`, matching every other
// fixture in this directory).

//go:embed testdata/async/stream_write_host_read.wasm
var streamWriteHostReadWasm []byte

//go:embed testdata/async/future_write_host_write.wasm
var futureWriteHostWriteWasm []byte

// TestAcceptance_GuestWritesStream_HostReads is acceptance case (a): a guest
// that WRITES a stream<u8> the host reads via a host StreamReader. The guest
// (testdata/async/stream_write_host_read.wat's "run" export):
//  1. calls stream.new to mint a fresh readable/writable pair,
//  2. passes the READABLE end to a synchronous import "sink(s: stream<u8>)",
//  3. calls stream.write on the WRITABLE end with 4 bytes {1,2,3,4} already
//     sitting in its memory.
//
// The host's "sink" HostFunc receives args[0] as a *StreamReader (host_import
// .go's resolveHandleArg StreamDesc case) and calls Read(4, onDone) BEFORE
// returning -- so by the time the guest's stream.write runs, a host read is
// already parked, and the rendezvous is synchronous and immediate.
func TestAcceptance_GuestWritesStream_HostReads(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	var gotBytes []abi.Value
	var gotRes CopyResult
	sinkParams := []binary.TypeDesc{binary.StreamDesc{Element: &binary.TypeRef{Primitive: "u8"}}}
	opt := WithImport("sink", "", func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		reader, ok := args[0].(*StreamReader)
		if !ok {
			t.Fatalf("sink arg is %T, want *StreamReader", args[0])
		}
		reader.Read(4, func(res CopyResult, vals []abi.Value) {
			gotRes = res
			gotBytes = vals
		})
		return nil, nil
	}, sinkParams, nil)

	inst, err := Instantiate(ctx, r, streamWriteHostReadWasm, opt)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	res, err := inst.Call(ctx, "run")
	if err != nil {
		t.Fatalf("Call(run): %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("Call(run): got %d result(s), want 1", len(res))
	}
	packed, ok := res[0].(uint32)
	if !ok {
		t.Fatalf("Call(run) result is %T, want uint32", res[0])
	}
	// packCopyResult(COMPLETED, 4) == 0 | 4<<4 == 0x40.
	if want := packCopyResult(copyCompleted, 4); packed != want {
		t.Fatalf("Call(run) = %#x, want %#x (COMPLETED, progress=4)", packed, want)
	}

	if gotRes != CopyCompleted {
		t.Fatalf("host Read result = %v, want CopyCompleted", gotRes)
	}
	if len(gotBytes) != 4 {
		t.Fatalf("host Read got %d byte(s), want 4: %v", len(gotBytes), gotBytes)
	}
	want := []uint32{1, 2, 3, 4}
	for i, w := range want {
		got, ok := gotBytes[i].(uint32)
		if !ok || got != w {
			t.Fatalf("byte %d = %v (%T), want %d", i, gotBytes[i], gotBytes[i], w)
		}
	}
}

// TestAcceptance_HostWritesFuture_GuestReads is acceptance case (b): a guest
// that reads a future<u32> the host writes via a host FutureWriter. The host
// creates the future (NewFuture), arranges Set(42, ...) BEFORE calling the
// guest export, then calls "process" with the future's readable end as the
// arg (instance.go's resolveArgHandlesDepth FutureDesc case mints the
// readable end in the guest's table). The guest
// (testdata/async/future_write_host_write.wat's "process" export) calls
// future.read and returns the u32 value it loaded from memory.
func TestAcceptance_HostWritesFuture_GuestReads(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, futureWriteHostWriteWasm)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	fw, readableVal, err := inst.NewFuture(binary.PrimitiveDesc{Prim: "u32"})
	if err != nil {
		t.Fatalf("NewFuture: %v", err)
	}
	var setRes CopyResult
	fw.Set(uint32(42), func(res CopyResult) { setRes = res })

	res, err := inst.Call(ctx, "process", readableVal)
	if err != nil {
		t.Fatalf("Call(process): %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("Call(process): got %d result(s), want 1", len(res))
	}
	got, ok := res[0].(uint32)
	if !ok {
		t.Fatalf("Call(process) result is %T, want uint32", res[0])
	}
	if got != 42 {
		t.Fatalf("Call(process) = %d, want 42", got)
	}
	if setRes != CopyCompleted {
		t.Fatalf("host Set result = %v, want CopyCompleted", setRes)
	}
}
