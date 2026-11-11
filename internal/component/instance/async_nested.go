package instance

import (
	"fmt"

	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// A stream, future or error-context is a handle the abi lifts and lowers as
// an opaque i32, wherever it sits. One at the top level of an argument or
// result crosses where its handle does (resolveHandleArg, allocHandleResult,
// resolveArgHandlesDepth, takeAsyncResult); one nested in a composite --
// wasi:http's result<tuple<stream<u8>, future<...>>> -- crosses through an
// asyncNest, which walks the value for it. Unlike a nested own or borrow,
// which the host gets as a handle in its own table, the host gets the value:
// a *StreamReader, *FutureReader or *ErrorContext, the one thing it can do
// anything with.

// asyncLeaves are the stream, future and error-context values walkNested
// visits.
var asyncLeaves = leaves{is: isAsyncValue, held: typeContainsAsyncValueNested}

func isAsyncValue(t binary.TypeDesc) bool {
	switch d := t.(type) {
	case binary.StreamDesc, binary.FutureDesc:
		return true
	case binary.PrimitiveDesc:
		return d.Prim == "error-context"
	}
	return false
}

// asyncNest moves the streams, futures and error contexts nested in values of
// type t across a call boundary. A type that holds none gets none, so the
// values of every other type cost nothing.
type asyncNest struct {
	t       binary.TypeDesc
	resolve abi.Resolver
}

func newAsyncNest(t binary.TypeDesc, resolve abi.Resolver) *asyncNest {
	if t == nil || !typeContainsAsyncValueNested(t, resolve, 0) {
		return nil
	}
	return &asyncNest{t: t, resolve: resolve}
}

// lift lifts each nested value of v out of the guest's table, for the host.
func (n *asyncNest) lift(in *Instance, table *handleTable, v abi.Value) (abi.Value, error) {
	return walkNested(v, n.t, n.resolve, 0, asyncLeaves, func(t binary.TypeDesc, v abi.Value) (abi.Value, error) {
		return liftAsyncValue(in, table, n.resolve, t, v)
	})
}

// lower lowers each nested value of v into the guest's table.
func (n *asyncNest) lower(table *handleTable, v abi.Value) (abi.Value, error) {
	return walkNested(v, n.t, n.resolve, 0, asyncLeaves, func(t binary.TypeDesc, v abi.Value) (abi.Value, error) {
		return lowerAsyncValue(table, t, v)
	})
}

// liftAsyncValue lifts the stream, future or error-context handle v, of type
// td, out of the guest's table: lift_async_value takes a readable end,
// trapping unless it is idle, not in a waitable set, and of the element type
// resolve gives td; lift_error_context reads the error context, the guest
// keeping its handle.
func liftAsyncValue(in *Instance, table *handleTable, resolve abi.Resolver, td binary.TypeDesc, v abi.Value) (abi.Value, error) {
	h, ok := v.(uint32)
	if !ok {
		return nil, fmt.Errorf("%s: expected a uint32 handle, got %T", td.Kind(), v)
	}
	switch d := td.(type) {
	case binary.StreamDesc:
		elem, err := elemDescOf(d.Element, resolve)
		if err != nil {
			return nil, err
		}
		shared, err := takeReadableStreamEnd(in, table, elem, resolve, h)
		if err != nil {
			return nil, err
		}
		return &StreamReader{in: in, shared: shared, state: copyIdle}, nil
	case binary.FutureDesc:
		elem, err := elemDescOf(d.Element, resolve)
		if err != nil {
			return nil, err
		}
		shared, err := takeReadableFutureEnd(in, table, elem, resolve, h)
		if err != nil {
			return nil, err
		}
		return &FutureReader{in: in, shared: shared, state: copyIdle}, nil
	}
	raw, _ := table.getEntry(h)
	ec, ok := raw.(*ErrorContext)
	if !ok {
		return nil, fmt.Errorf("handle %d is not an error-context", h)
	}
	return ec, nil
}

// lowerAsyncValue lowers v, a stream's or future's reader or an error
// context, into the guest's table: a readable end for the stream or future,
// a handle sharing the error context (or a new one, for a string).
func lowerAsyncValue(table *handleTable, td binary.TypeDesc, v abi.Value) (abi.Value, error) {
	switch td.(type) {
	case binary.StreamDesc:
		switch sv := v.(type) {
		case *sharedStream:
			return table.addEntry(&streamEnd{side: sideReadable, state: copyIdle, shared: sv}), nil
		case *StreamReader:
			return table.addEntry(&streamEnd{side: sideReadable, state: copyIdle, shared: sv.shared}), nil
		}
		return nil, fmt.Errorf("stream: expected a *StreamReader, got %T", v)
	case binary.FutureDesc:
		switch sv := v.(type) {
		case *sharedFuture:
			return table.addEntry(&futureEnd{side: sideReadable, state: copyIdle, shared: sv}), nil
		case *FutureReader:
			return table.addEntry(&futureEnd{side: sideReadable, state: copyIdle, shared: sv.shared}), nil
		}
		return nil, fmt.Errorf("future: expected a *FutureReader, got %T", v)
	}
	switch ev := v.(type) {
	case *ErrorContext:
		return table.addEntry(ev), nil
	case string:
		return table.addEntry(&ErrorContext{debugMessage: ev}), nil
	}
	return nil, fmt.Errorf("error-context: expected a *ErrorContext or string, got %T", v)
}

func elemDescOf(elem *binary.TypeRef, resolve abi.Resolver) (binary.TypeDesc, error) {
	if elem == nil {
		return nil, nil
	}
	d, err := resolveTypeRef(elem, resolve)
	if err != nil {
		return nil, fmt.Errorf("element type: %w", err)
	}
	return d, nil
}
