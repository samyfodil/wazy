package wasi_otel

import (
	"fmt"
	"time"

	"github.com/samyfodil/wazy/component"
)

// Lifting the WIT values into the Go types.
//
// The type graph is deep -- a metrics export nests eight records inside four
// lists -- so instead of every step returning an error, a lifter carries the
// first one and every later step reports a zero value. That keeps the shape of
// the code the same as the shape of the WIT, and one check at the end decides
// whether the call traps. Nothing here guesses: a value of the wrong Go type
// means the declared signature and the engine disagree, which is a bug in this
// package rather than anything a guest can cause, and it is reported as such.
type lifter struct {
	err error
}

// fail records the first error. Later failures are dropped: the first one is
// the cause, and the rest are its consequences.
func (l *lifter) fail(format string, args ...any) {
	if l.err == nil {
		l.err = fmt.Errorf(format, args...)
	}
}

// fields opens a record (or tuple) of exactly n fields.
func (l *lifter) fields(v component.Value, n int, what string) []component.Value {
	if l.err != nil {
		return make([]component.Value, n)
	}
	f, ok := v.([]component.Value)
	if !ok {
		l.fail("%s: expected a record, got %T", what, v)
		return make([]component.Value, n)
	}
	if len(f) != n {
		l.fail("%s: expected %d fields, got %d", what, n, len(f))
		return make([]component.Value, n)
	}
	return f
}

// list opens a list. A nil value is an empty list, not an error: `none` is
// distinguished by the option layer above, which never calls this.
func (l *lifter) list(v component.Value, what string) []component.Value {
	if l.err != nil || v == nil {
		return nil
	}
	items, ok := v.([]component.Value)
	if !ok {
		l.fail("%s: expected a list, got %T", what, v)
		return nil
	}
	return items
}

// some unwraps an option. present is false for `none`.
//
// An empty list lifts to a non-nil empty slice and `none` lifts to nil, so the
// two never collide.
func (l *lifter) some(v component.Value) (component.Value, bool) {
	if l.err != nil || v == nil {
		return nil, false
	}
	return v, true
}

// variant opens a variant, returning its discriminant and payload.
func (l *lifter) variant(v component.Value, what string) (uint32, component.Value) {
	if l.err != nil {
		return 0, nil
	}
	vv, ok := v.(component.VariantValue)
	if !ok {
		l.fail("%s: expected a variant, got %T", what, v)
		return 0, nil
	}
	return vv.Disc, vv.Payload
}

func (l *lifter) str(v component.Value, what string) string {
	if l.err != nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		l.fail("%s: expected a string, got %T", what, v)
		return ""
	}
	return s
}

func (l *lifter) bool_(v component.Value, what string) bool {
	if l.err != nil {
		return false
	}
	b, ok := v.(bool)
	if !ok {
		l.fail("%s: expected a bool, got %T", what, v)
		return false
	}
	return b
}

// u32 reads a u8, u16, u32, enum or flags value, all of which lift as uint32.
func (l *lifter) u32(v component.Value, what string) uint32 {
	if l.err != nil {
		return 0
	}
	u, ok := v.(uint32)
	if !ok {
		l.fail("%s: expected a u32, got %T", what, v)
		return 0
	}
	return u
}

func (l *lifter) u64(v component.Value, what string) uint64 {
	if l.err != nil {
		return 0
	}
	u, ok := v.(uint64)
	if !ok {
		l.fail("%s: expected a u64, got %T", what, v)
		return 0
	}
	return u
}

// s32 reads an s8, s16 or s32 value, all of which lift as int32.
func (l *lifter) s32(v component.Value, what string) int32 {
	if l.err != nil {
		return 0
	}
	i, ok := v.(int32)
	if !ok {
		l.fail("%s: expected an s32, got %T", what, v)
		return 0
	}
	return i
}

func (l *lifter) s64(v component.Value, what string) int64 {
	if l.err != nil {
		return 0
	}
	i, ok := v.(int64)
	if !ok {
		l.fail("%s: expected an s64, got %T", what, v)
		return 0
	}
	return i
}

func (l *lifter) f64(v component.Value, what string) float64 {
	if l.err != nil {
		return 0
	}
	f, ok := v.(float64)
	if !ok {
		l.fail("%s: expected an f64, got %T", what, v)
		return 0
	}
	return f
}

// datetime lifts wasi:clocks/wall-clock's `datetime` record.
func (l *lifter) datetime(v component.Value, what string) time.Time {
	f := l.fields(v, 2, what)
	secs := l.u64(f[0], what+".seconds")
	nanos := l.u32(f[1], what+".nanoseconds")
	if l.err != nil {
		return time.Time{}
	}
	return time.Unix(int64(secs), int64(nanos)).UTC()
}

// optDatetime lifts an option<datetime>.
func (l *lifter) optDatetime(v component.Value, what string) *time.Time {
	inner, ok := l.some(v)
	if !ok {
		return nil
	}
	t := l.datetime(inner, what)
	return &t
}

// optStr lifts an option<string>.
func (l *lifter) optStr(v component.Value, what string) *string {
	inner, ok := l.some(v)
	if !ok {
		return nil
	}
	s := l.str(inner, what)
	return &s
}

// keyValues lifts a list<key-value>.
func (l *lifter) keyValues(v component.Value, what string) []KeyValue {
	items := l.list(v, what)
	if items == nil {
		return nil
	}
	out := make([]KeyValue, len(items))
	for i, item := range items {
		f := l.fields(item, 2, fmt.Sprintf("%s[%d]", what, i))
		out[i] = KeyValue{
			Key:   l.str(f[0], what+".key"),
			Value: l.str(f[1], what+".value"),
		}
	}
	return out
}

// resource lifts types.wit's `%resource`.
func (l *lifter) resource(v component.Value, what string) Resource {
	f := l.fields(v, 2, what)
	return Resource{
		Attributes: l.keyValues(f[0], what+".attributes"),
		SchemaURL:  l.optStr(f[1], what+".schema-url"),
	}
}

// scope lifts types.wit's `instrumentation-scope`.
func (l *lifter) scope(v component.Value, what string) InstrumentationScope {
	f := l.fields(v, 4, what)
	return InstrumentationScope{
		Name:       l.str(f[0], what+".name"),
		Version:    l.optStr(f[1], what+".version"),
		SchemaURL:  l.optStr(f[2], what+".schema-url"),
		Attributes: l.keyValues(f[3], what+".attributes"),
	}
}
