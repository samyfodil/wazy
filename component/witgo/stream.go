package witgo

import (
	"fmt"

	"github.com/samyfodil/wazy/component"
)

// StreamReader is the readable end of a WIT stream<T>, which is how the
// bindings pass every stream: an import's argument or result, an export's
// argument or result. The host reads one it was given, or hands one to the
// guest, after which it is the guest's.
//
// Its operations follow component.StreamReader's: they run on the instance's
// scheduler, and complete through a callback.
type StreamReader[T any] struct {
	r    *component.StreamReader
	lift func(component.Value) (T, error)
}

// StreamWriter is the host's writable end of a stream<T>, made with its
// reader by NewStream.
type StreamWriter[T any] struct {
	w     *component.StreamWriter
	lower func(T) component.Value
}

// NewStream makes a stream<T> of inst's, whose element type elem resolve
// resolves (nil for a bare stream), converting its values with lift and
// lower.
func NewStream[T any](inst *component.Instance, elem *component.TypeRef, resolve component.Resolver, lift func(component.Value) (T, error), lower func(T) component.Value) (*StreamWriter[T], *StreamReader[T], error) {
	w, r, err := inst.NewStreamOf(elem, resolve)
	if err != nil {
		return nil, nil, err
	}
	return &StreamWriter[T]{w: w, lower: lower}, &StreamReader[T]{r: r, lift: lift}, nil
}

// ReaderOf is the StreamReader the bindings lift from v, a stream an import
// received or an export returned, as a *component.StreamReader.
func ReaderOf[T any](v component.Value, lift func(component.Value) (T, error)) (*StreamReader[T], error) {
	r, err := want[*component.StreamReader](v, "stream")
	if err != nil {
		return nil, err
	}
	return &StreamReader[T]{r: r, lift: lift}, nil
}

// Raw is the untyped reader, which the bindings pass to the engine. It is nil
// for a nil s.
func (s *StreamReader[T]) Raw() component.Value {
	if s == nil {
		return nil
	}
	return s.r
}

// Read reads up to n values. onDone gets what was read, and how the read
// ended: CopyDropped once the writer has closed the stream.
func (s *StreamReader[T]) Read(n uint32, onDone func(res component.CopyResult, vals []T, err error)) {
	s.r.Read(n, func(res component.CopyResult, vs []component.Value) {
		vals, err := liftAll(vs, s.lift)
		onDone(res, vals, err)
	})
}

// ReadAll reads until the writer closes the stream, chunk values at a time,
// and gives onDone everything read. A read cancelled first ends it with an
// error.
func (s *StreamReader[T]) ReadAll(chunk uint32, onDone func(vals []T, err error)) {
	var all []T
	var next func()
	next = func() {
		s.Read(chunk, func(res component.CopyResult, vals []T, err error) {
			all = append(all, vals...)
			switch {
			case err != nil:
				onDone(all, err)
			case res == component.CopyDropped:
				onDone(all, nil)
			case res == component.CopyCancelled:
				onDone(all, fmt.Errorf("witgo: stream read cancelled"))
			default:
				next()
			}
		})
	}
	next()
}

// Cancel revokes a pending Read, which then completes as CopyCancelled.
func (s *StreamReader[T]) Cancel() { s.r.Cancel() }

// Close drops the reader: the writer's pending and later writes complete as
// CopyDropped.
func (s *StreamReader[T]) Close() { s.r.Close() }

// Write offers vals to the stream. onDone gets how many the reader took, and
// how the write ended: CopyDropped once the reader has closed the stream. One
// write may be pending at a time.
func (s *StreamWriter[T]) Write(vals []T, onDone func(res component.CopyResult, n uint32)) {
	s.w.Write(lowerAll(vals, s.lower), onDone)
}

// WriteAll writes every value of vals, as the reader takes them, and gives
// onDone how many it took and how the last write ended.
func (s *StreamWriter[T]) WriteAll(vals []T, onDone func(n int, res component.CopyResult)) {
	vs := lowerAll(vals, s.lower)
	done := 0
	var next func()
	next = func() {
		s.w.Write(vs[done:], func(res component.CopyResult, n uint32) {
			done += int(n)
			if res != component.CopyCompleted || done == len(vs) {
				onDone(done, res)
				return
			}
			next()
		})
	}
	next()
}

// Cancel revokes a pending Write, which then completes as CopyCancelled.
func (s *StreamWriter[T]) Cancel() { s.w.Cancel() }

// Close ends the stream: the reader's pending and later reads complete as
// CopyDropped.
func (s *StreamWriter[T]) Close() { s.w.Close() }

// FutureReader is the readable end of a WIT future<T>; see StreamReader.
type FutureReader[T any] struct {
	r    *component.FutureReader
	lift func(component.Value) (T, error)
}

// FutureWriter is the host's writable end of a future<T>, made with its
// reader by NewFuture.
type FutureWriter[T any] struct {
	w     *component.FutureWriter
	lower func(T) component.Value
}

// NewFuture is NewStream for a future<T>.
func NewFuture[T any](inst *component.Instance, elem *component.TypeRef, resolve component.Resolver, lift func(component.Value) (T, error), lower func(T) component.Value) (*FutureWriter[T], *FutureReader[T], error) {
	w, r, err := inst.NewFutureOf(elem, resolve)
	if err != nil {
		return nil, nil, err
	}
	return &FutureWriter[T]{w: w, lower: lower}, &FutureReader[T]{r: r, lift: lift}, nil
}

// FutureReaderOf is ReaderOf for a future.
func FutureReaderOf[T any](v component.Value, lift func(component.Value) (T, error)) (*FutureReader[T], error) {
	r, err := want[*component.FutureReader](v, "future")
	if err != nil {
		return nil, err
	}
	return &FutureReader[T]{r: r, lift: lift}, nil
}

// Raw is the untyped reader, which the bindings pass to the engine. It is nil
// for a nil f.
func (f *FutureReader[T]) Raw() component.Value {
	if f == nil {
		return nil
	}
	return f.r
}

// Get reads the future's value, once it is set. onDone gets CopyDropped,
// and no value, if the writer closed it unset.
func (f *FutureReader[T]) Get(onDone func(res component.CopyResult, v T, err error)) {
	f.r.Get(func(res component.CopyResult, v component.Value) {
		var x T
		var err error
		if res == component.CopyCompleted {
			x, err = f.lift(v)
		}
		onDone(res, x, err)
	})
}

// Cancel revokes a pending Get, which then completes as CopyCancelled.
func (f *FutureReader[T]) Cancel() { f.r.Cancel() }

// Close drops the reader.
func (f *FutureReader[T]) Close() { f.r.Close() }

// Set sets the future's value. onDone runs once the reader has it, or as
// CopyDropped if the reader closed first.
func (f *FutureWriter[T]) Set(v T, onDone func(res component.CopyResult)) {
	f.w.Set(f.lower(v), onDone)
}

// Close drops the writer, which may only follow a completed Set.
func (f *FutureWriter[T]) Close() { f.w.Close() }

// ErrorContextOf reads an error-context an import received or an export
// returned.
func ErrorContextOf(v component.Value) (*component.ErrorContext, error) {
	return want[*component.ErrorContext](v, "error-context")
}

// LowerErrorContext writes e, which is nil for a nil e: the engine then
// refuses it, rather than passing an error context with no message.
func LowerErrorContext(e *component.ErrorContext) component.Value {
	if e == nil {
		return nil
	}
	return e
}

// Unit reads a bare stream's or future's element, which has no value.
func Unit(component.Value) (struct{}, error) { return struct{}{}, nil }

// LowerUnit writes a bare stream's or future's element.
func LowerUnit(struct{}) component.Value { return nil }

func liftAll[T any](vs []component.Value, lift func(component.Value) (T, error)) ([]T, error) {
	out := make([]T, len(vs))
	for i, v := range vs {
		x, err := lift(v)
		if err != nil {
			return out[:i], err
		}
		out[i] = x
	}
	return out, nil
}

func lowerAll[T any](vals []T, lower func(T) component.Value) []component.Value {
	out := make([]component.Value, len(vals))
	for i, v := range vals {
		out[i] = lower(v)
	}
	return out
}
