package component

import (
	"fmt"
	"reflect"
)

// ListOf returns a list Value as a []T.
//
// A list of a fixed-width primitive lifts to the Go slice of that primitive
// (see ListDesc), so a host function reading one can take it directly. Lists
// of anything else -- strings, records, nested lists -- lift as []Value, and a
// caller wanting []string or []MyRecord has to walk them. ListOf does both:
//
//	names, err := component.ListOf[string](args[0])   // list<string>
//	sizes, err := component.ListOf[uint32](args[1])   // list<u32>
//
// The typed shape costs nothing -- it is returned as it arrived, not copied --
// so this is not a slow compatibility wrapper for the case it was written for.
//
// It also accepts the []Value shape for a scalar list, which is what a host
// function written before lists were typed will have been asserting, and what
// a caller building one by hand still produces. Converting between the two is
// what the slower path does, element by element.
//
// An element that is not a T, and cannot be converted to one, is an error
// naming its index: a list is not a place to guess.
func ListOf[T any](v Value) ([]T, error) {
	// Already the requested shape. This is the whole point of the typed rule
	// and has to stay allocation-free.
	if typed, ok := v.([]T); ok {
		return typed, nil
	}

	vals, ok := v.([]Value)
	if !ok {
		var zero T
		return nil, fmt.Errorf("component: expected a list of %T, got %T", zero, v)
	}

	out := make([]T, len(vals))
	target := reflect.TypeOf(out).Elem()
	for i, e := range vals {
		if t, ok := e.(T); ok {
			out[i] = t
			continue
		}
		// A scalar arrives in the element Value shape, which is widened: a
		// u16 element is a uint32, an s8 an int32. Converting narrows it back
		// to the T the caller asked for, and cannot lose anything, since the
		// value came from a field of that width.
		ev := reflect.ValueOf(e)
		if !ev.IsValid() || !ev.CanConvert(target) {
			var zero T
			return nil, fmt.Errorf("component: list element %d is %T, want %T", i, e, zero)
		}
		out[i] = ev.Convert(target).Interface().(T)
	}
	return out, nil
}
