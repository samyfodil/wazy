package witgo

import (
	"context"
	_ "embed"
	"testing"
	"time"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/component"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// borrow.wasm imports one host resource, test:lend/host.r, which is all a
// handle table needs; async.wasm's fetch awaits the async import slow and
// returns what it gives plus one (testdata/asyncguest).
var (
	//go:embed testdata/borrow.wasm
	borrowWasm []byte
	//go:embed testdata/async.wasm
	asyncWasm []byte
)

const tag uint32 = 0x700

func table(t *testing.T) *component.HandleTable {
	t.Helper()
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	t.Cleanup(func() { r.Close(ctx) })
	var tbl *component.HandleTable
	inst, err := component.Instantiate(ctx, r, borrowWasm,
		component.WithResourceTag("test:lend/host", "r", tag),
		component.WithResourcesHook(func(h *component.HandleTable) { tbl = h }))
	require.NoError(t, err)
	t.Cleanup(func() { inst.Close(ctx) })
	return tbl
}

func TestHandles(t *testing.T) {
	h := &Handles{Table: table(t)}

	own := h.Mint(tag, 5, false)
	rep, err := h.Rep(tag, false, own)
	require.NoError(t, err)
	require.Equal(t, uint32(5), rep)
	rep, err = h.Rep(tag, true, own)
	require.NoError(t, err)
	require.Equal(t, uint32(5), rep)
	_, err = h.Rep(tag, false, own)
	require.Error(t, err, "an own read takes the handle out of the table")
	_, err = h.Rep(tag, false, "x")
	require.EqualError(t, err, "witgo: u32: got string")

	lent := h.Mint(tag, 6, true)
	require.NoError(t, h.Release())
	_, err = h.Table.Rep(tag, lent.(uint32))
	require.Error(t, err, "a lent handle is dropped by Release")
	require.NoError(t, h.Release(), "nothing left to release")

	lent = h.Mint(tag, 7, true)
	require.NoError(t, h.Table.Drop(tag, lent.(uint32)))
	require.Error(t, h.Release(), "a lent handle already gone is reported")
}

// An async import completed through Async -- on the scheduler with Defer, and
// from another goroutine -- and an export awaited through Pending.
func TestAsyncAndPending(t *testing.T) {
	ctx := context.Background()
	u32 := []component.TypeDesc{component.PrimitiveDesc{Prim: "u32"}}
	for _, external := range []bool{false, true} {
		r := wazy.NewRuntime(ctx)
		gate := make(chan struct{})
		inst, err := component.Instantiate(ctx, r, asyncWasm, component.WithAsyncImport("wazy:async-probe/host@0.1.0", "slow",
			func(_ context.Context, args []component.Value, call *component.AsyncCall) error {
				a := NewAsync(call, func(v uint32) []component.Value { return []component.Value{v} })
				a.OnCancel(func() {})
				x := args[0].(uint32)
				if external {
					go func() { <-gate; a.Return(x * 10) }()
				} else {
					a.Defer(func() { a.Return(x * 10) })
				}
				return nil
			}, u32, u32))
		require.NoError(t, err)

		p, err := inst.CallAsync(ctx, "fetch", uint32(4))
		require.NoError(t, err)
		pp := NewPending(p, func(res []component.Value) (uint32, error) { return U32(res[0]) })
		if external {
			select {
			case <-pp.Done():
				t.Fatal("returned before the import completed")
			case <-time.After(10 * time.Millisecond):
			}
			close(gate)
		}
		v, err := pp.Await(ctx)
		require.NoError(t, err)
		require.Equal(t, uint32(41), v)
		<-pp.Done()
		inst.Close(ctx)
		r.Close(ctx)
	}

	// A cancelled call's Await reports the cancellation.
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	inst, err := component.Instantiate(ctx, r, asyncWasm, component.WithAsyncImport("wazy:async-probe/host@0.1.0", "slow",
		func(context.Context, []component.Value, *component.AsyncCall) error { return nil }, u32, u32))
	require.NoError(t, err)
	defer inst.Close(ctx)
	p, err := inst.CallAsync(ctx, "fetch", uint32(1))
	require.NoError(t, err)
	pp := NewPending(p, func(res []component.Value) (uint32, error) { return U32(res[0]) })
	require.NoError(t, pp.Cancel(ctx))
	_, err = pp.Await(ctx)
	require.Error(t, err)
}
