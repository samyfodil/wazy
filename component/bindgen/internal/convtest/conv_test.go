package convtest

import (
	"testing"

	"github.com/samyfodil/wazy/component"
	"github.com/samyfodil/wazy/component/witgo"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// The generated conversions for types no guest fixture reaches: what lower
// makes is the component.Value shape the ABI takes (see abi.Value), and lift
// reads it back to the value it came from.

func holder() Holder {
	return Holder{
		M:         map[string]Item{"k": {ID: 1, Tags: []string{"a"}}},
		F:         [2]Item{{ID: 2, Tags: []string{}}, {ID: 3, Tags: []string{"b", "c"}}},
		O:         &[]Node{NodeLeaf(7), NodePair(witgo.Tuple2[int16, string]{F0: -2, F1: "p"}), NodeNone()},
		Okonly:    witgo.Ok[uint64, struct{}](9),
		Bare:      witgo.Err[struct{}](struct{}{}),
		Errorly:   witgo.Err[struct{}](LevelHigh),
		T:         witgo.Tuple3[bool, rune, float32]{F0: true, F1: 'é', F2: 0.5},
		B:         BitsA | BitsD,
		L:         LevelHigh,
		Nested:    [][]uint16{{1, 2}, {}},
		Fixedprim: [2]int8{-1, 1},
		Ints:      []int32{-5},
		Chars:     []rune{'x'},
		Bools:     []bool{true, false},
		Long:      Tuple9{1, -1, 2, -2, 3, -3, 4, -4, "nine"},
	}
}

func TestLowerShape(t *testing.T) {
	got := lowerHolder(nil, holder())
	want := []component.Value{
		[]component.Value{[]component.Value{"k", []component.Value{uint32(1), []component.Value{"a"}}}},
		[]component.Value{
			[]component.Value{uint32(2), []component.Value{}},
			[]component.Value{uint32(3), []component.Value{"b", "c"}},
		},
		[]component.Value{
			component.VariantValue{Disc: 0, Payload: uint32(7)},
			component.VariantValue{Disc: 1, Payload: []component.Value{int32(-2), "p"}},
			component.VariantValue{Disc: 2},
		},
		component.ResultValue{Payload: uint64(9)},
		component.ResultValue{IsErr: true},
		component.ResultValue{IsErr: true, Payload: uint32(1)},
		[]component.Value{true, 'é', float32(0.5)},
		uint32(0b1001),
		uint32(1),
		[]component.Value{[]uint16{1, 2}, []uint16{}},
		[]int8{-1, 1},
		[]int32{-5},
		[]rune{'x'},
		[]bool{true, false},
		[]component.Value{uint32(1), int32(-1), uint32(2), int32(-2), uint32(3), int32(-3), uint64(4), int64(-4), "nine"},
	}
	require.Equal(t, want, got)
}

func TestRoundtrip(t *testing.T) {
	// A nil list lifts back as an empty one, so the zero case holds empty ones.
	empty := Holder{
		M: map[string]Item{}, F: [2]Item{{Tags: []string{}}, {Tags: []string{}}},
		Nested: [][]uint16{}, Ints: []int32{}, Chars: []rune{}, Bools: []bool{},
	}
	for _, h := range []Holder{holder(), empty} {
		got, err := liftHolder(nil, lowerHolder(nil, h))
		require.NoError(t, err)
		require.Equal(t, h, got)
	}
	m := map[uint32][]byte{4: {1}, 5: {}}
	got, err := liftT14(nil, lowerT14(nil, m))
	require.NoError(t, err)
	require.Equal(t, m, got)
	o, err := liftOtherItem(nil, lowerOtherItem(nil, OtherItem{X: 255}))
	require.NoError(t, err)
	require.Equal(t, OtherItem{X: 255}, o)
}

// A value of the wrong shape fails to lift, naming what it expected, rather
// than panicking or reading garbage.
func TestLiftRejects(t *testing.T) {
	for name, f := range map[string]func() error{
		"record arity": func() error { _, err := liftItem(nil, []component.Value{uint32(1)}); return err },
		"record field": func() error { _, err := liftItem(nil, []component.Value{"x", []component.Value{}}); return err },
		"variant case": func() error { _, err := liftNode(nil, component.VariantValue{Disc: 3}); return err },
		"variant type": func() error { _, err := liftNode(nil, uint32(0)); return err },
		"enum range":   func() error { _, err := liftLevel(nil, uint32(2)); return err },
		"fixed length": func() error { _, err := liftT12(nil, []int8{1}); return err },
		"fixed items":  func() error { _, err := liftT3(nil, []component.Value{}); return err },
		"map entry":    func() error { _, err := liftT2(nil, []component.Value{[]component.Value{"k"}}); return err },
		"result":       func() error { _, err := liftT7(nil, uint64(1)); return err },
		"option":       func() error { _, err := liftT6(nil, "x"); return err },
		"tuple":        func() error { _, err := liftT10(nil, []component.Value{true, 'a'}); return err },
		"list item":    func() error { _, err := liftT5(nil, []component.Value{uint32(1)}); return err },
	} {
		require.Error(t, f(), name)
	}
}

// A variant case accessor answers only for its own case.
func TestVariantAccessors(t *testing.T) {
	_, ok := NodeLeaf(1).Pair()
	require.False(t, ok)
	v, ok := NodeLeaf(1).Leaf()
	require.True(t, ok)
	require.Equal(t, uint8(1), v)
	require.Equal(t, NodeCaseNone, NodeNone().Case)
}
