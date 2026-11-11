package kitchen

import (
	"context"
	_ "embed"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/component/witgo"
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
	nextTok  Token
	dropTok  []Token

	// external makes the async imports complete from another goroutine,
	// after gate is closed when it is set; pings counts SlowPing.
	external bool
	gate     chan struct{}
	pings    int
}

// complete runs fn now, or from another goroutine once the gate opens.
func (h *host) complete(fn func()) {
	if !h.external {
		fn()
		return
	}
	go func() {
		if h.gate != nil {
			<-h.gate
		}
		fn()
	}()
}

func (h *host) SlowAdd(_ context.Context, a, b uint32, ret witgo.Async[uint32]) error {
	h.complete(func() { ret.Return(a + b) })
	return nil
}

func (h *host) SlowShape(ctx context.Context, s Shape, ret witgo.Async[witgo.Result[Color, string]]) error {
	r, _ := h.Classify(ctx, s)
	h.complete(func() { ret.Return(r) })
	return nil
}

func (h *host) SlowPing(_ context.Context, ret witgo.Async[struct{}]) error {
	h.pings++
	h.complete(func() { ret.Return(struct{}{}) })
	return nil
}

func (h *host) SlowMint(ctx context.Context, a, b uint32, ret witgo.Async[witgo.Tuple2[Token, Token]]) error {
	pair, _ := h.MintPair(ctx, a, b)
	h.complete(func() { ret.Return(pair) })
	return nil
}

// Mint and Peek are host's, DropToken types': the token resource is declared
// in types, which has no functions, and used only by host's.
func (h *host) Mint(_ context.Context, v uint32) (Token, error) {
	h.nextTok++
	h.tokens[h.nextTok] = v
	return h.nextTok, nil
}

// Deepen adds one to a some(some(n)), and returns none and some(none) as
// they came.
func (h *host) Deepen(_ context.Context, x **uint32) (**uint32, error) {
	if x == nil || *x == nil {
		return x, nil
	}
	n := **x + 1
	p := &n
	return &p, nil
}

func (h *host) MintPair(ctx context.Context, a, b uint32) (witgo.Tuple2[Token, Token], error) {
	x, _ := h.Mint(ctx, a)
	y, _ := h.Mint(ctx, b)
	return witgo.Tuple2[Token, Token]{F0: x, F1: y}, nil
}

func (h *host) PeekAll(ctx context.Context, ts []Token) (uint32, error) {
	var sum uint32
	for _, t := range ts {
		v, err := h.Peek(ctx, t)
		if err != nil {
			return 0, err
		}
		sum += v
	}
	return sum, nil
}

func (h *host) TryMint(ctx context.Context, v uint32) (witgo.Result[Token, string], error) {
	if v == 0 {
		return witgo.Err[Token]("zero"), nil
	}
	t, _ := h.Mint(ctx, v)
	return witgo.Ok[Token, string](t), nil
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
	k, err := Instantiate(ctx, r, kitchenWasm, Imports{Types: h, Host: h})
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

// Tokens nested in a host import's tuple result, list<borrow> argument and
// result<own> result cross as handles the bindings translate through the
// table, and every one the guest drops reaches DropToken.
func TestNestedImportHandles(t *testing.T) {
	k, h := instantiate(t)
	got, err := k.API().PairTokens(context.Background(), 2, 3)
	require.NoError(t, err)
	require.Equal(t, uint32(2+3+7+1), got)
	require.Equal(t, 3, len(h.dropTok))
	require.Equal(t, 0, len(h.tokens))
}

// The host's own tokens passed into exports: owned ones the guest drops reach
// DropToken; a borrowed one is released after the call with the host still
// owning it; one the guest returns comes back as the host's rep.
func TestHostHandlesInExports(t *testing.T) {
	k, h := instantiate(t)
	ctx := context.Background()
	api := k.API()
	mint := func(v uint32) Token { tok, _ := h.Mint(ctx, v); return tok }

	a, b := mint(10), mint(20)
	sum, err := api.Tally(ctx, []Token{a, b})
	require.NoError(t, err)
	require.Equal(t, uint32(30), sum)
	require.Equal(t, []Token{a, b}, h.dropTok)

	c := mint(5)
	got, err := api.TakeToken(ctx, c)
	require.NoError(t, err)
	require.Equal(t, uint32(5), got)
	require.Equal(t, []Token{a, b, c}, h.dropTok)

	d := mint(8)
	for range 2 { // the lent handle is released, so it can be lent again
		got, err = api.PeekToken(ctx, d)
		require.NoError(t, err)
		require.Equal(t, uint32(8), got)
	}
	require.Equal(t, 3, len(h.dropTok), "a borrow does not drop the host's token")
	require.Equal(t, uint32(8), h.tokens[d])

	e, err := api.GiveToken(ctx, 9)
	require.NoError(t, err)
	require.Equal(t, uint32(9), h.tokens[e], "the token the guest returned is the host's rep")
	require.Equal(t, 3, len(h.dropTok))
}

// A resource the guest implements: constructed, used through its methods and
// a static, returned in a list, borrowed by another export, and dropped.
func TestExportedResource(t *testing.T) {
	k, _ := instantiate(t)
	ctx := context.Background()
	api := k.API()

	a, err := api.NewBlob(ctx, "ab")
	require.NoError(t, err)
	require.NoError(t, a.Append(ctx, "c"))
	s, err := a.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, "abc", s)

	b, err := api.NewBlob(ctx, "xy")
	require.NoError(t, err)
	j, err := api.BlobJoin(ctx, a, b)
	require.NoError(t, err)
	s, err = j.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, "abcxy", s)
	n, err := api.BlobSize(ctx, j)
	require.NoError(t, err)
	require.Equal(t, uint32(5), n)

	many, err := api.MakeBlobs(ctx, 3)
	require.NoError(t, err)
	require.Equal(t, 3, len(many))
	for i, m := range many {
		s, err := m.Read(ctx)
		require.NoError(t, err)
		require.Equal(t, fmt.Sprintf("b%d", i), s)
		require.NoError(t, m.Drop(ctx))
	}
	for _, x := range []Blob{a, b, j} {
		require.NoError(t, x.Drop(ctx))
	}
	_, err = a.Read(ctx)
	require.Error(t, err, "a dropped handle is gone")
}

// option<option<u32>> keeps none and some(none) apart, into the guest, out to
// the host's import, and back.
func TestNestedOption(t *testing.T) {
	k, _ := instantiate(t)
	ctx := context.Background()
	five := uint32(5)
	pfive := &five
	var none *uint32

	got, err := k.API().Nest(ctx, nil)
	require.NoError(t, err)
	require.Nil(t, got)

	got, err = k.API().Nest(ctx, &none)
	require.NoError(t, err)
	require.NotNil(t, got, "some(none) came back as none")
	require.Nil(t, *got)

	got, err = k.API().Nest(ctx, &pfive)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, *got)
	require.Equal(t, uint32(6), **got)
}

// Async imports completed before they return, through the blocking forms of
// the async exports.
func TestAsyncBlocking(t *testing.T) {
	k, h := instantiate(t)
	ctx := context.Background()
	api := k.API()

	sum, err := api.Fetch(ctx, 2, 3)
	require.NoError(t, err)
	require.Equal(t, uint32(5), sum)

	c, err := api.FetchShape(ctx, ShapeCircle(1))
	require.NoError(t, err)
	require.Equal(t, witgo.Ok[Color, string](ColorRed), c)
	c, err = api.FetchShape(ctx, ShapeEmpty())
	require.NoError(t, err)
	require.Equal(t, witgo.Err[Color]("empty"), c)

	n, err := api.Fire(ctx)
	require.NoError(t, err)
	require.Equal(t, uint32(2), n)
	require.Equal(t, 2, h.pings)

	tok, err := api.FetchTokens(ctx, 3, 4)
	require.NoError(t, err)
	require.Equal(t, uint32(7), tok)
	require.Equal(t, 2, len(h.dropTok))

	b, err := api.NewBlob(ctx, "abcd")
	require.NoError(t, err)
	m, err := b.Measure(ctx)
	require.NoError(t, err)
	require.Equal(t, uint32(4), m)
	require.NoError(t, b.Drop(ctx))

	top, err := k.Top(ctx, "x")
	require.NoError(t, err)
	require.Equal(t, "top:x", top)
}

// Async imports completed from other goroutines, through the CallAsync forms:
// the call stays pending until the host completes it.
func TestAsyncExternal(t *testing.T) {
	k, h := instantiate(t)
	h.external = true
	ctx := context.Background()
	api := k.API()

	h.gate = make(chan struct{})
	p, err := api.FetchAsync(ctx, 20, 22)
	require.NoError(t, err)
	select {
	case <-p.Done():
		t.Fatal("the call returned before the host completed its import")
	case <-time.After(20 * time.Millisecond):
	}
	close(h.gate)
	sum, err := p.Await(ctx)
	require.NoError(t, err)
	require.Equal(t, uint32(42), sum)
	h.gate = nil

	ps, err := api.FetchShapeAsync(ctx, ShapeRect(Point{}))
	require.NoError(t, err)
	c, err := ps.Await(ctx)
	require.NoError(t, err)
	require.Equal(t, witgo.Ok[Color, string](ColorGreen), c)

	pf, err := api.FireAsync(ctx)
	require.NoError(t, err)
	n, err := pf.Await(ctx)
	require.NoError(t, err)
	require.Equal(t, uint32(2), n)

	pt, err := api.FetchTokensAsync(ctx, 5, 6)
	require.NoError(t, err)
	tok, err := pt.Await(ctx)
	require.NoError(t, err)
	require.Equal(t, uint32(11), tok)
	require.Equal(t, 2, len(h.dropTok))

	b, err := api.NewBlob(ctx, "xyz")
	require.NoError(t, err)
	pm, err := b.MeasureAsync(ctx)
	require.NoError(t, err)
	m, err := pm.Await(ctx)
	require.NoError(t, err)
	require.Equal(t, uint32(3), m)
	require.NoError(t, b.Drop(ctx))

	ptop, err := k.TopAsync(ctx, "y")
	require.NoError(t, err)
	top, err := ptop.Await(ctx)
	require.NoError(t, err)
	require.Equal(t, "top:y", top)
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
