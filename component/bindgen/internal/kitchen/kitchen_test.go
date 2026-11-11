package kitchen

import (
	"context"
	_ "embed"
	"fmt"
	"math"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/component/witgo"
	"github.com/samyfodil/wazy/imports/wasip2"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// kitchen.wasm is guest/, built by guest/build.sh against guest/wit.
//
//go:embed kitchen.wasm
var kitchenWasm []byte

type host struct {
	counters map[Counter]uint32
	next     Counter
	dropped  []Counter
	echoed   []Everything
	tokens   map[Token]uint32
	dropTok  []Token
}

// Mint and Peek are host's, DropToken types': the token resource is declared
// in types, which has no functions, and used only by host's.
func (h *host) Mint(_ context.Context, v uint32) (Token, error) {
	t := Token(len(h.tokens) + len(h.dropTok) + 1)
	h.tokens[t] = v
	return t, nil
}

func (h *host) Peek(_ context.Context, t Token) (uint32, error) {
	v, ok := h.tokens[t]
	if !ok {
		return 0, fmt.Errorf("no token %d", t)
	}
	return v, nil
}

func (h *host) DropToken(_ context.Context, t Token) error {
	h.dropTok = append(h.dropTok, t)
	delete(h.tokens, t)
	return nil
}

func (h *host) NewCounter(_ context.Context, start uint32) (Counter, error) {
	h.next++
	h.counters[h.next] = start
	return h.next, nil
}

func (h *host) CounterBump(_ context.Context, self Counter, by uint32) (uint32, error) {
	v, ok := h.counters[self]
	if !ok {
		return 0, fmt.Errorf("no counter %d", self)
	}
	v += by
	h.counters[self] = v
	return v, nil
}

func (h *host) CounterZero(ctx context.Context) (Counter, error) { return h.NewCounter(ctx, 0) }

func (h *host) DropCounter(_ context.Context, self Counter) error {
	h.dropped = append(h.dropped, self)
	delete(h.counters, self)
	return nil
}

func (h *host) Echo(_ context.Context, e Everything) (Everything, error) {
	h.echoed = append(h.echoed, e)
	return e, nil
}

func (h *host) Classify(_ context.Context, s Shape) (witgo.Result[Color, string], error) {
	switch s.Case {
	case ShapeCaseCircle:
		return witgo.Ok[Color, string](ColorRed), nil
	case ShapeCaseRect:
		return witgo.Ok[Color, string](ColorGreen), nil
	}
	return witgo.Err[Color]("empty"), nil
}

func (h *host) Checked(_ context.Context, n uint32) (witgo.Result[struct{}, string], error) {
	if n > 100 {
		return witgo.Err[struct{}](fmt.Sprintf("%d is too big", n)), nil
	}
	return witgo.Ok[struct{}, string](struct{}{}), nil
}

func (h *host) Pick(_ context.Context, c Color, p Perms) (*string, error) {
	if p == 0 {
		return nil, nil
	}
	s := fmt.Sprintf("%d:%03b", c, p)
	return &s, nil
}

func instantiate(t *testing.T) (*Kitchen, *host) {
	t.Helper()
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	t.Cleanup(func() { r.Close(ctx) })
	h := &host{counters: map[Counter]uint32{}, tokens: map[Token]uint32{}}
	k, err := Instantiate(ctx, r, kitchenWasm, Imports{Types: h, Host: h}, wasip2.WithWASI(wasip2.WASIConfig{})...)
	require.NoError(t, err)
	t.Cleanup(func() { k.Close(ctx) })
	return k, h
}

func ptr[T any](v T) *T { return &v }

// Every field of everything crosses to the guest, out to the host's echo,
// back, and back to the test, at the edges of its range.
func TestRoundtrip(t *testing.T) {
	k, h := instantiate(t)
	ctx := context.Background()
	for _, e := range []Everything{
		{
			B: true, Tiny: math.MinInt8, Utiny: math.MaxUint8, Short: math.MinInt16, Ushort: math.MaxUint16,
			Int: math.MinInt32, Uint: math.MaxUint32, Long: math.MinInt64, Ulong: math.MaxUint64,
			Single: -1.5, Double: math.MaxFloat64, C: '世', S: "héllo",
			Bytes: []byte{0, 1, 255}, Words: []string{"a", "", "ç"},
			Points: []Point{{X: -1, Y: 2, Label: "p"}, {X: math.MaxInt32, Y: math.MinInt32}},
			Maybe:  &Point{X: 7, Label: "m"}, Nothing: ptr[uint32](0),
			Pair: witgo.Tuple2[uint32, string]{F0: 9, F1: "nine"}, Fixed: [3]uint16{1, 0, math.MaxUint16},
			Col: ColorBlue, Per: PermsRead | PermsExec,
			Shapes: []Shape{ShapeCircle(2.5), ShapeRect(Point{X: 3, Y: 4, Label: "r"}), ShapeEmpty()},
		},
		{Bytes: []byte{}, Words: []string{}, Points: []Point{}, Shapes: []Shape{}},
	} {
		got, err := k.API().Roundtrip(ctx, e)
		require.NoError(t, err)
		want := e
		want.S += "!"
		require.Equal(t, want, got)
		require.Equal(t, e, h.echoed[len(h.echoed)-1], "the host received what the test sent")
	}
}

func TestVariantResultOptionTuple(t *testing.T) {
	k, _ := instantiate(t)
	ctx := context.Background()
	api := k.API()

	for _, c := range []struct {
		s    Shape
		want float64
	}{{ShapeCircle(2), 12}, {ShapeRect(Point{X: 3, Y: 5}), 15}, {ShapeEmpty(), 0}} {
		got, err := api.Area(ctx, c.s)
		require.NoError(t, err)
		require.Equal(t, c.want, got)
	}

	ok, err := api.Check(ctx, 21)
	require.NoError(t, err)
	require.Equal(t, witgo.Ok[uint32, string](42), ok)
	bad, err := api.Check(ctx, 101)
	require.NoError(t, err)
	require.Equal(t, witgo.Err[uint32]("101 is too big"), bad)

	first, err := api.First(ctx, []Point{{X: 1, Label: "one"}, {X: 2}})
	require.NoError(t, err)
	require.Equal(t, &Point{X: 1, Label: "one"}, first)
	none, err := api.First(ctx, nil)
	require.NoError(t, err)
	require.Nil(t, none)

	sw, err := api.Swap(ctx, witgo.Tuple2[string, uint32]{F0: "x", F1: 5})
	require.NoError(t, err)
	require.Equal(t, witgo.Tuple2[uint32, string]{F0: 5, F1: "x"}, sw)

	all, err := api.ClassifyAll(ctx, []Shape{ShapeCircle(1), ShapeEmpty(), ShapeRect(Point{})})
	require.NoError(t, err)
	require.Equal(t, []witgo.Result[Color, string]{
		witgo.Ok[Color, string](ColorRed), witgo.Err[Color]("empty"), witgo.Ok[Color, string](ColorGreen),
	}, all)

	name, err := api.PickName(ctx, ColorGreen, PermsWrite|PermsRead)
	require.NoError(t, err)
	require.Equal(t, ptr("1:011"), name)
	noName, err := api.PickName(ctx, ColorRed, 0)
	require.NoError(t, err)
	require.Nil(t, noName)

	require.NoError(t, api.Nothing(ctx))

	pong, err := k.Ping(ctx, "hi")
	require.NoError(t, err)
	require.Equal(t, "pong:hi", pong)
}

// The guest constructs a host counter and a static zero, bumps both, and
// drops both, which reaches DropCounter.
func TestResource(t *testing.T) {
	k, h := instantiate(t)
	got, err := k.API().UseCounter(context.Background(), 5, 3)
	require.NoError(t, err)
	require.Equal(t, uint32(8), got)
	require.Equal(t, 2, len(h.dropped))
	require.Equal(t, 0, len(h.counters), "every counter the guest made was dropped")
}

// Tokens minted by host's functions are registered, and dropped, through
// types, the interface that declares them.
func TestCrossInterfaceResource(t *testing.T) {
	k, h := instantiate(t)
	got, err := k.API().UseTokens(context.Background(), 4, 6)
	require.NoError(t, err)
	require.Equal(t, uint32(10), got)
	require.Equal(t, 2, len(h.dropTok))
	require.Equal(t, 0, len(h.tokens), "every token the guest held was dropped")
}

// Variant accessors report the case they were asked about only.
func TestVariantAccessors(t *testing.T) {
	r, ok := ShapeCircle(1).Rect()
	require.False(t, ok)
	require.Equal(t, Point{}, r)
	c, ok := ShapeCircle(1).Circle()
	require.True(t, ok)
	require.Equal(t, 1.0, c)
}
