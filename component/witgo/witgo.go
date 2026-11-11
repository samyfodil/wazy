// Package witgo holds the types and conversions that Go bindings generated
// by wazy-bindgen (see package bindgen) are written in terms of: WIT's result
// and tuple types, and the reading of a component.Value into the Go type a
// WIT primitive maps to.
package witgo

import (
	"context"
	"fmt"

	"github.com/samyfodil/wazy/component"
)

// Result is WIT's result<T, E>. An arm WIT leaves out is struct{}.
type Result[T, E any] struct {
	Ok    T
	Err   E
	IsErr bool
}

// Ok is a Result holding v as its ok arm.
func Ok[T, E any](v T) Result[T, E] { return Result[T, E]{Ok: v} }

// Err is a Result holding e as its error arm.
func Err[T, E any](e E) Result[T, E] { return Result[T, E]{Err: e, IsErr: true} }

// Tuple2 to Tuple8 are WIT's tuples, their elements in order.
type (
	Tuple2[T0, T1 any] struct {
		F0 T0
		F1 T1
	}
	Tuple3[T0, T1, T2 any] struct {
		F0 T0
		F1 T1
		F2 T2
	}
	Tuple4[T0, T1, T2, T3 any] struct {
		F0 T0
		F1 T1
		F2 T2
		F3 T3
	}
	Tuple5[T0, T1, T2, T3, T4 any] struct {
		F0 T0
		F1 T1
		F2 T2
		F3 T3
		F4 T4
	}
	Tuple6[T0, T1, T2, T3, T4, T5 any] struct {
		F0 T0
		F1 T1
		F2 T2
		F3 T3
		F4 T4
		F5 T5
	}
	Tuple7[T0, T1, T2, T3, T4, T5, T6 any] struct {
		F0 T0
		F1 T1
		F2 T2
		F3 T3
		F4 T4
		F5 T5
		F6 T6
	}
	Tuple8[T0, T1, T2, T3, T4, T5, T6, T7 any] struct {
		F0 T0
		F1 T1
		F2 T2
		F3 T3
		F4 T4
		F5 T5
		F6 T6
		F7 T7
	}
)

// MaxTuple is the longest tuple witgo has a type for.
const MaxTuple = 8

func want[T any](v component.Value, wit string) (T, error) {
	t, ok := v.(T)
	if !ok {
		var zero T
		return zero, fmt.Errorf("witgo: %s: got %T", wit, v)
	}
	return t, nil
}

// Bool reads a bool.
func Bool(v component.Value) (bool, error) { return want[bool](v, "bool") }

// S8 reads an s8, which lifts as an int32.
func S8(v component.Value) (int8, error) {
	x, err := want[int32](v, "s8")
	return int8(x), err
}

// S16 reads an s16, which lifts as an int32.
func S16(v component.Value) (int16, error) {
	x, err := want[int32](v, "s16")
	return int16(x), err
}

// S32 reads an s32.
func S32(v component.Value) (int32, error) { return want[int32](v, "s32") }

// S64 reads an s64.
func S64(v component.Value) (int64, error) { return want[int64](v, "s64") }

// U8 reads a u8, which lifts as a uint32.
func U8(v component.Value) (uint8, error) {
	x, err := want[uint32](v, "u8")
	return uint8(x), err
}

// U16 reads a u16, which lifts as a uint32.
func U16(v component.Value) (uint16, error) {
	x, err := want[uint32](v, "u16")
	return uint16(x), err
}

// U32 reads a u32, and an enum's case, a flags value or a resource handle,
// which lift as one.
func U32(v component.Value) (uint32, error) { return want[uint32](v, "u32") }

// U64 reads a u64.
func U64(v component.Value) (uint64, error) { return want[uint64](v, "u64") }

// F32 reads an f32.
func F32(v component.Value) (float32, error) { return want[float32](v, "f32") }

// F64 reads an f64.
func F64(v component.Value) (float64, error) { return want[float64](v, "f64") }

// Char reads a char.
func Char(v component.Value) (rune, error) { return want[rune](v, "char") }

// String reads a string.
func String(v component.Value) (string, error) { return want[string](v, "string") }

// Values reads a list, record or tuple that lifts as a []component.Value,
// checking its length when n is not negative.
func Values(v component.Value, n int, wit string) ([]component.Value, error) {
	vs, err := want[[]component.Value](v, wit)
	if err == nil && n >= 0 && len(vs) != n {
		return nil, fmt.Errorf("witgo: %s: %d values, want %d", wit, len(vs), n)
	}
	return vs, err
}

// Variant reads a variant.
func Variant(v component.Value, wit string) (component.VariantValue, error) {
	return want[component.VariantValue](v, wit)
}

// ResultValue reads a result.
func ResultValue(v component.Value, wit string) (component.ResultValue, error) {
	return want[component.ResultValue](v, wit)
}

// Handles moves resource handles between a component's handle table and the
// reps the host works with, for the handles the engine does not translate
// itself: those nested in an import's composite arguments and results, and
// every one an export takes or returns.
type Handles struct {
	// Table is the handle table the handles live in.
	Table *component.HandleTable
	// Inst is the instance a guest resource's handle belongs to.
	Inst *component.Instance

	lent []lent
}

type lent struct{ tag, handle uint32 }

// Rep reads the handle v of the host resource tag, returning its rep: taking
// the handle out of the table for an own, leaving it for a borrow.
func (h *Handles) Rep(tag uint32, own bool, v component.Value) (uint32, error) {
	handle, err := U32(v)
	if err != nil {
		return 0, err
	}
	if own {
		return h.Table.TakeOwn(tag, handle)
	}
	return h.Table.Rep(tag, handle)
}

// Mint puts rep, a host resource of tag, in the table and returns the handle.
// One minted for a borrow is dropped again by Release, once the call that
// borrows it has returned.
func (h *Handles) Mint(tag, rep uint32, borrow bool) component.Value {
	handle := h.Table.NewOwn(tag, rep)
	if borrow {
		h.lent = append(h.lent, lent{tag, handle})
	}
	return handle
}

// Release drops the handles minted for borrows. It does not run the
// resources' destructors: the host still owns them.
func (h *Handles) Release() error {
	var first error
	for _, l := range h.lent {
		if err := h.Table.Drop(l.tag, l.handle); err != nil && first == nil {
			first = err
		}
	}
	h.lent = nil
	return first
}

// Async completes one call of an async import: the host may return its result
// before the import function returns, or later -- from another goroutine,
// when the call was started with CallAsync.
type Async[T any] struct {
	call  *component.AsyncCall
	lower func(T) []component.Value
}

// NewAsync is the Async generated code hands an async import's host
// function, lowering its result with lower.
func NewAsync[T any](call *component.AsyncCall, lower func(T) []component.Value) Async[T] {
	return Async[T]{call: call, lower: lower}
}

// Return completes the call with v. It may be called once.
func (a Async[T]) Return(v T) { a.call.Resolve(a.lower(v)) }

// Cancelled completes the call as cancelled, once the guest has asked to
// cancel it (see OnCancel).
func (a Async[T]) Cancelled() { a.call.ResolveCancelled() }

// OnCancel registers fn to run if the guest cancels the call.
func (a Async[T]) OnCancel(fn func()) { a.call.OnCancel(fn) }

// Defer runs fn on the instance's scheduler, where Return may be called
// without CallAsync.
func (a Async[T]) Defer(fn func()) { a.call.Defer(fn) }

// Pending is an async export call in flight, started with CallAsync.
type Pending[T any] struct {
	p    *component.PendingCall
	lift func([]component.Value) (T, error)
}

// NewPending is the Pending generated code returns, lifting the call's
// results with lift.
func NewPending[T any](p *component.PendingCall, lift func([]component.Value) (T, error)) *Pending[T] {
	return &Pending[T]{p: p, lift: lift}
}

// Await drives the call until it returns, and gives its result.
func (p *Pending[T]) Await(ctx context.Context) (T, error) {
	res, err := p.p.Await(ctx)
	if err != nil {
		var zero T
		return zero, err
	}
	return p.lift(res)
}

// Done is closed once the call has returned.
func (p *Pending[T]) Done() <-chan struct{} { return p.p.Done() }

// Cancel asks the guest to cancel the call.
func (p *Pending[T]) Cancel(ctx context.Context) error { return p.p.Cancel(ctx) }
