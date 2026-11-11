package witgo

import (
	"context"
	"errors"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/component"
	"github.com/samyfodil/wazy/internal/testing/require"
)

func instance(t *testing.T) *component.Instance {
	t.Helper()
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	t.Cleanup(func() { r.Close(ctx) })
	inst, err := component.Instantiate(ctx, r, borrowWasm, component.WithResourceTag("test:lend/host", "r", tag))
	require.NoError(t, err)
	t.Cleanup(func() { inst.Close(ctx) })
	return inst
}

var u32Ref = component.Prim("u32")

func lowerU32(x uint32) component.Value { return x }

func u32Stream(t *testing.T, inst *component.Instance) (*StreamWriter[uint32], *StreamReader[uint32]) {
	t.Helper()
	w, r, err := NewStream(inst, &u32Ref, nil, U32, lowerU32)
	require.NoError(t, err)
	return w, r
}

// Both ends held by the host: what one writes the other reads, in as many
// copies as the reader asks for, until the writer closes.
func TestStreamReadAll(t *testing.T) {
	w, r := u32Stream(t, instance(t))
	var got []uint32
	var gotErr error
	done := false
	r.ReadAll(2, func(vals []uint32, err error) { got, gotErr, done = vals, err, true })
	wrote, res := -1, component.CopyResult(99)
	w.WriteAll([]uint32{1, 2, 3, 4, 5}, func(n int, rs component.CopyResult) { wrote, res = n, rs })
	require.Equal(t, 5, wrote)
	require.Equal(t, component.CopyCompleted, res)
	require.False(t, done)
	w.Close()
	require.True(t, done)
	require.NoError(t, gotErr)
	require.Equal(t, []uint32{1, 2, 3, 4, 5}, got)
}

func TestStreamReadWrite(t *testing.T) {
	w, r := u32Stream(t, instance(t))
	var n uint32
	w.Write([]uint32{7, 8}, func(_ component.CopyResult, k uint32) { n = k })
	var got []uint32
	r.Read(1, func(res component.CopyResult, vals []uint32, err error) {
		require.NoError(t, err)
		require.Equal(t, component.CopyCompleted, res)
		got = vals
	})
	require.Equal(t, []uint32{7}, got)
	require.Equal(t, uint32(1), n)
}

// A value the element type cannot read fails the read with what it read
// before it.
func TestStreamLiftError(t *testing.T) {
	inst := instance(t)
	bad := errors.New("bad")
	w, r, err := NewStream(inst, &u32Ref, nil, func(v component.Value) (uint32, error) {
		if v.(uint32) == 2 {
			return 0, bad
		}
		return v.(uint32), nil
	}, lowerU32)
	require.NoError(t, err)
	w.Write([]uint32{1, 2, 3}, func(component.CopyResult, uint32) {})
	var got []uint32
	var gotErr error
	r.ReadAll(8, func(vals []uint32, err error) { got, gotErr = vals, err })
	require.ErrorIs(t, gotErr, bad)
	require.Equal(t, []uint32{1}, got)
}

// Cancelling, or the other end going away, ends a copy with how far it got.
func TestStreamCancelAndDrop(t *testing.T) {
	inst := instance(t)

	_, r := u32Stream(t, inst)
	var gotErr error
	r.ReadAll(1, func(_ []uint32, err error) { gotErr = err })
	r.Cancel()
	require.EqualError(t, gotErr, "witgo: stream read cancelled")

	w, r := u32Stream(t, inst)
	res := component.CopyResult(99)
	w.Write([]uint32{1}, func(rs component.CopyResult, _ uint32) { res = rs })
	w.Cancel()
	require.Equal(t, component.CopyCancelled, res)

	r.Close()
	wrote := -1
	w.WriteAll([]uint32{1, 2}, func(n int, rs component.CopyResult) { wrote, res = n, rs })
	require.Equal(t, 0, wrote)
	require.Equal(t, component.CopyDropped, res)
}

func TestFuture(t *testing.T) {
	inst := instance(t)
	w, r, err := NewFuture(inst, &u32Ref, nil, U32, lowerU32)
	require.NoError(t, err)
	set := component.CopyResult(99)
	w.Set(42, func(rs component.CopyResult) { set = rs })
	var got uint32
	r.Get(func(res component.CopyResult, v uint32, err error) {
		require.NoError(t, err)
		require.Equal(t, component.CopyCompleted, res)
		got = v
	})
	require.Equal(t, uint32(42), got)
	require.Equal(t, component.CopyCompleted, set)
	w.Close()

	// The reader gone, a value set is dropped; one not yet set is no value.
	w, r, err = NewFuture(inst, &u32Ref, nil, U32, lowerU32)
	require.NoError(t, err)
	r.Close()
	w.Set(1, func(rs component.CopyResult) { set = rs })
	require.Equal(t, component.CopyDropped, set)

	w, r, err = NewFuture(inst, &u32Ref, nil, func(component.Value) (uint32, error) { return 0, errors.New("bad") }, lowerU32)
	require.NoError(t, err)
	var gotErr error
	r.Get(func(_ component.CopyResult, _ uint32, err error) { gotErr = err })
	w.Set(1, func(component.CopyResult) {})
	require.EqualError(t, gotErr, "bad")

	_, r, err = NewFuture(inst, &u32Ref, nil, U32, lowerU32)
	require.NoError(t, err)
	res := component.CopyResult(99)
	r.Get(func(rs component.CopyResult, v uint32, err error) {
		res = rs
		require.NoError(t, err)
		require.Zero(t, v)
	})
	r.Cancel()
	require.Equal(t, component.CopyCancelled, res)
}

// A bare stream's elements carry nothing.
func TestUnitStream(t *testing.T) {
	w, r, err := NewStream(instance(t), nil, nil, Unit, LowerUnit)
	require.NoError(t, err)
	w.WriteAll(make([]struct{}, 3), func(int, component.CopyResult) { w.Close() })
	var got []struct{}
	r.ReadAll(8, func(vals []struct{}, err error) {
		require.NoError(t, err)
		got = vals
	})
	require.Equal(t, 3, len(got))
}

func TestStreamLiftShapes(t *testing.T) {
	inst := instance(t)
	_, err := ReaderOf("x", U32)
	require.EqualError(t, err, "witgo: stream: got string")
	_, err = FutureReaderOf(uint32(1), U32)
	require.EqualError(t, err, "witgo: future: got uint32")
	_, err = ErrorContextOf(uint32(1))
	require.EqualError(t, err, "witgo: error-context: got uint32")
	e := component.NewErrorContext("m")
	got, err := ErrorContextOf(e)
	require.NoError(t, err)
	require.Equal(t, e, got)
	require.Equal(t, component.Value(e), LowerErrorContext(e))
	require.Nil(t, LowerErrorContext(nil))

	idx := component.TypeRef{TypeIndex: new(uint32)}
	_, _, err = NewStream(inst, &idx, nil, U32, lowerU32)
	require.EqualError(t, err, "stream/future element type: no resolver for type index")
	_, _, err = NewFuture(inst, &idx, nil, U32, lowerU32)
	require.EqualError(t, err, "stream/future element type: no resolver for type index")

	var sr *StreamReader[uint32]
	var fr *FutureReader[uint32]
	require.Nil(t, sr.Raw())
	require.Nil(t, fr.Raw())
	v, err := Unit("anything")
	require.NoError(t, err)
	require.Equal(t, struct{}{}, v)
	require.Nil(t, LowerUnit(struct{}{}))

	_, r := u32Stream(t, inst)
	rv, ok := r.Raw().(*component.StreamReader)
	require.True(t, ok)
	require.NotNil(t, rv)
	r.Close()
}
