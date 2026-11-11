package witgo

import (
	"testing"

	"github.com/samyfodil/wazy/component"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// Each reader takes the Go type its WIT type lifts as -- the narrow integers
// as the 32-bit one, per abi.Value -- and refuses anything else, naming the
// WIT type it wanted.
func TestReaders(t *testing.T) {
	for _, c := range []struct {
		name    string
		read    func(component.Value) (any, error)
		in, out any
	}{
		{"bool", func(v component.Value) (any, error) { return Bool(v) }, true, true},
		{"s8", func(v component.Value) (any, error) { return S8(v) }, int32(-8), int8(-8)},
		{"s16", func(v component.Value) (any, error) { return S16(v) }, int32(-16), int16(-16)},
		{"s32", func(v component.Value) (any, error) { return S32(v) }, int32(-32), int32(-32)},
		{"s64", func(v component.Value) (any, error) { return S64(v) }, int64(-64), int64(-64)},
		{"u8", func(v component.Value) (any, error) { return U8(v) }, uint32(8), uint8(8)},
		{"u16", func(v component.Value) (any, error) { return U16(v) }, uint32(16), uint16(16)},
		{"u32", func(v component.Value) (any, error) { return U32(v) }, uint32(32), uint32(32)},
		{"u64", func(v component.Value) (any, error) { return U64(v) }, uint64(64), uint64(64)},
		{"f32", func(v component.Value) (any, error) { return F32(v) }, float32(1.5), float32(1.5)},
		{"f64", func(v component.Value) (any, error) { return F64(v) }, 2.5, 2.5},
		{"char", func(v component.Value) (any, error) { return Char(v) }, 'x', 'x'},
		{"string", func(v component.Value) (any, error) { return String(v) }, "s", "s"},
	} {
		got, err := c.read(c.in)
		require.NoError(t, err, c.name)
		require.Equal(t, c.out, got, c.name)
		_, err = c.read(struct{}{})
		require.EqualError(t, err, "witgo: "+c.name+": got struct {}", c.name)
	}
}

func TestComposites(t *testing.T) {
	vs, err := Values([]component.Value{uint32(1), "a"}, 2, "record")
	require.NoError(t, err)
	require.Equal(t, []component.Value{uint32(1), "a"}, vs)
	_, err = Values([]component.Value{uint32(1)}, 2, "record")
	require.EqualError(t, err, "witgo: record: 1 values, want 2")
	_, err = Values(uint32(1), -1, "list")
	require.EqualError(t, err, "witgo: list: got uint32")
	all, err := Values([]component.Value{}, -1, "list")
	require.NoError(t, err)
	require.Equal(t, 0, len(all))

	vv, err := Variant(component.VariantValue{Disc: 1, Payload: "p"}, "v")
	require.NoError(t, err)
	require.Equal(t, component.VariantValue{Disc: 1, Payload: "p"}, vv)
	_, err = Variant(uint32(0), "v")
	require.EqualError(t, err, "witgo: v: got uint32")

	rv, err := ResultValue(component.ResultValue{IsErr: true}, "r")
	require.NoError(t, err)
	require.True(t, rv.IsErr)
	_, err = ResultValue(nil, "r")
	require.EqualError(t, err, "witgo: r: got <nil>")

	require.Equal(t, Result[int, string]{Ok: 1}, Ok[int, string](1))
	require.Equal(t, Result[int, string]{Err: "e", IsErr: true}, Err[int]("e"))
}
