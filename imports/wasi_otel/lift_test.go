package wasi_otel

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/samyfodil/wazy/component"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// The lifter's failure arms. A guest cannot reach them -- the engine has
// already decoded to the declared types by the time a value arrives -- so they
// only fire when this package's declared type table and its lifting disagree.
// That is exactly the bug worth catching loudly, and these are the tests that
// say what it looks like.

func TestLifter_typeMismatches(t *testing.T) {
	// Each case hands a lifter the wrong Go type and expects it to say so
	// rather than return a plausible zero.
	tests := []struct {
		name string
		lift func(l *lifter)
	}{
		{"fields", func(l *lifter) { l.fields("not a record", 2, "x") }},
		{"list", func(l *lifter) { l.list("not a list", "x") }},
		{"variant", func(l *lifter) { l.variant("not a variant", "x") }},
		{"str", func(l *lifter) { l.str(42, "x") }},
		{"bool", func(l *lifter) { l.bool_("no", "x") }},
		{"u32", func(l *lifter) { l.u32("no", "x") }},
		{"u64", func(l *lifter) { l.u64("no", "x") }},
		{"s32", func(l *lifter) { l.s32("no", "x") }},
		{"s64", func(l *lifter) { l.s64("no", "x") }},
		{"f64", func(l *lifter) { l.f64("no", "x") }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := &lifter{}
			tc.lift(l)
			require.Error(t, l.err)
			require.Contains(t, l.err.Error(), "x")
		})
	}
}

// A record arriving with the wrong number of fields is a layout disagreement,
// which has to be reported rather than indexed past.
func TestLifter_wrongFieldCount(t *testing.T) {
	l := &lifter{}
	f := l.fields([]component.Value{"only one"}, 2, "rec")
	require.Error(t, l.err)
	require.Contains(t, l.err.Error(), "expected 2 fields, got 1")
	// The returned slice is still the requested width, so a caller indexing
	// into it cannot panic on the way to the error check.
	require.Equal(t, 2, len(f))
}

// Only the first error is kept: the ones after it are its consequences, and
// reporting the last would point at the wrong field.
func TestLifter_keepsFirstError(t *testing.T) {
	l := &lifter{}
	l.str(1, "first")
	l.str(2, "second")
	require.Contains(t, l.err.Error(), "first")
	require.False(t, strings.Contains(l.err.Error(), "second"),
		"the later failure should not replace the first: %v", l.err)
}

// Once failed, every accessor returns a zero value without adding an error, so
// a lift of a deep record unwinds to one message instead of dozens.
func TestLifter_shortCircuits(t *testing.T) {
	l := &lifter{err: errors.New("already failed")}

	require.Equal(t, 2, len(l.fields("anything", 2, "x")))
	require.Nil(t, l.list([]component.Value{"a"}, "x"))
	require.Equal(t, "", l.str("real string", "x"))
	require.False(t, l.bool_(true, "x"))
	require.Equal(t, uint32(0), l.u32(uint32(9), "x"))
	require.Equal(t, uint64(0), l.u64(uint64(9), "x"))
	require.Equal(t, int32(0), l.s32(int32(9), "x"))
	require.Equal(t, int64(0), l.s64(int64(9), "x"))
	require.Equal(t, float64(0), l.f64(9.5, "x"))
	require.Equal(t, time.Time{}, l.datetime([]component.Value{uint64(1), uint32(0)}, "x"))
	require.Nil(t, l.keyValues([]component.Value{}, "x"))
	require.Nil(t, l.optStr("s", "x"))
	require.Nil(t, l.optDatetime([]component.Value{uint64(1), uint32(0)}, "x"))

	disc, payload := l.variant(component.VariantValue{Disc: 3}, "x")
	require.Equal(t, uint32(0), disc)
	require.Nil(t, payload)

	_, ok := l.some("value")
	require.False(t, ok)

	// The error is still the original one.
	require.Equal(t, "already failed", l.err.Error())
}

// none and an empty list are different values, and the difference is what
// LogRecord.Attributes uses to mean "absent" rather than "empty".
func TestLifter_someDistinguishesNoneFromEmpty(t *testing.T) {
	l := &lifter{}

	_, ok := l.some(nil)
	require.False(t, ok, "nil is none")

	empty, ok := l.some([]component.Value{})
	require.True(t, ok, "an empty list is some, not none")
	require.NotNil(t, empty)

	// And an empty list lifts to an empty, non-nil result.
	require.Equal(t, 0, len(l.list(empty, "x")))
	require.NoError(t, l.err)
}

// A nil list is treated as empty rather than an error: only the option layer
// above distinguishes absent, and it never passes nil down.
func TestLifter_nilList(t *testing.T) {
	l := &lifter{}
	require.Nil(t, l.list(nil, "x"))
	require.Nil(t, l.keyValues(nil, "x"))
	require.NoError(t, l.err)
}

func TestLifter_datetime(t *testing.T) {
	l := &lifter{}
	got := l.datetime([]component.Value{uint64(1700000000), uint32(250)}, "t")
	require.NoError(t, l.err)
	require.Equal(t, int64(1700000000), got.Unix())
	require.Equal(t, 250, got.Nanosecond())
	// Recorded times must not depend on the embedder's zone.
	require.Equal(t, "UTC", got.Location().String())

	// A malformed one reports which field was wrong.
	bad := &lifter{}
	bad.datetime([]component.Value{"nope", uint32(0)}, "t")
	require.Error(t, bad.err)
	require.Contains(t, bad.err.Error(), "t.seconds")
}

func TestLifter_optionals(t *testing.T) {
	l := &lifter{}

	require.Nil(t, l.optStr(nil, "x"))
	s := l.optStr("here", "x")
	require.NotNil(t, s)
	require.Equal(t, "here", *s)

	require.Nil(t, l.optDatetime(nil, "x"))
	d := l.optDatetime([]component.Value{uint64(5), uint32(6)}, "x")
	require.NotNil(t, d)
	require.Equal(t, int64(5), d.Unix())

	require.NoError(t, l.err)
}

func TestLifter_keyValues(t *testing.T) {
	l := &lifter{}
	got := l.keyValues([]component.Value{
		[]component.Value{"a", "1"},
		[]component.Value{"b", "2"},
	}, "attrs")
	require.NoError(t, l.err)
	require.Equal(t, 2, len(got))
	require.Equal(t, KeyValue{Key: "a", Value: "1"}, got[0])
	require.Equal(t, KeyValue{Key: "b", Value: "2"}, got[1])

	// A malformed pair names the index it was at.
	bad := &lifter{}
	bad.keyValues([]component.Value{"not a pair"}, "attrs")
	require.Error(t, bad.err)
	require.Contains(t, bad.err.Error(), "attrs[0]")
}

func TestLifter_resourceAndScope(t *testing.T) {
	l := &lifter{}

	res := l.resource([]component.Value{
		[]component.Value{[]component.Value{"k", "v"}},
		"https://schema",
	}, "res")
	require.NoError(t, l.err)
	require.Equal(t, 1, len(res.Attributes))
	require.NotNil(t, res.SchemaURL)
	require.Equal(t, "https://schema", *res.SchemaURL)

	sc := l.scope([]component.Value{
		"name", nil, nil, []component.Value{},
	}, "scope")
	require.NoError(t, l.err)
	require.Equal(t, "name", sc.Name)
	require.Nil(t, sc.Version)
	require.Nil(t, sc.SchemaURL)
	require.Equal(t, 0, len(sc.Attributes))
}

// A variant discriminant outside the declared cases means the type table and
// the guest disagree, so it is reported rather than silently clamped.
func TestLift_variantOutOfRange(t *testing.T) {
	t.Run("metric-number", func(t *testing.T) {
		l := &lifter{}
		liftNumber(l, component.VariantValue{Disc: 9}, "value")
		require.Error(t, l.err)
		require.Contains(t, l.err.Error(), "case 9 is out of range")
	})

	t.Run("metric-data", func(t *testing.T) {
		l := &lifter{}
		liftMetricData(l, component.VariantValue{Disc: 12})
		require.Error(t, l.err)
		require.Contains(t, l.err.Error(), "case 12 is out of range")
	})

	t.Run("metric-data on an already-failed lift", func(t *testing.T) {
		l := &lifter{err: errors.New("earlier")}
		got := liftMetricData(l, component.VariantValue{Disc: 0})
		require.Equal(t, MetricData{}, got)
		require.Equal(t, "earlier", l.err.Error())
	})
}

// liftNumber covers each arm, since the kind decides which field a consumer
// reads.
func TestLift_number(t *testing.T) {
	tests := []struct {
		name     string
		value    component.Value
		expected Number
	}{
		{"f64", component.VariantValue{Disc: 0, Payload: 1.5}, Number{Kind: NumberF64, F64: 1.5}},
		{"s64", component.VariantValue{Disc: 1, Payload: int64(-2)}, Number{Kind: NumberS64, S64: -2}},
		{"u64", component.VariantValue{Disc: 2, Payload: uint64(7)}, Number{Kind: NumberU64, U64: 7}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := &lifter{}
			got := liftNumber(l, tc.value, "value")
			require.NoError(t, l.err)
			require.Equal(t, tc.expected, got)
		})
	}

	t.Run("option none", func(t *testing.T) {
		l := &lifter{}
		require.Nil(t, liftOptNumber(l, nil, "min"))
		require.NoError(t, l.err)
	})
}

// errArgs guards a call arriving with an argument count the declared signature
// forbids, which would mean the engine and this package disagree.
func TestErrArgs(t *testing.T) {
	err := errArgs("on-end", 1, 3)
	require.Contains(t, err.Error(), "wasi:otel on-end")
	require.Contains(t, err.Error(), "expected 1 args, got 3")
}
