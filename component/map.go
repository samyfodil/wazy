package component

import (
	"fmt"
	"reflect"
)

// MapOf returns a map Value as a Go map[K]V.
//
// A map<K,V> lifts as a []Value of two-element []Value{key, value} pairs --
// the same shape list<tuple<K,V>> lifts to, since a map has no Canonical ABI
// representation of its own (see MapDesc). MapOf walks that shape and builds
// the Go map a host function actually wants to index:
//
//	counts, err := component.MapOf[string, uint32](args[0]) // map<string, u32>
//
// A key or value that is not a K/V and cannot be converted to one (the same
// widened-scalar case ListOf handles, e.g. a u16 value arriving as uint32) is
// an error naming the offending entry.
//
// map<K, option<V>> is read by instantiating K or V as a pointer:
// MapOf[string, *uint32] on a map<string, option<u32>> gives a nil *uint32 for
// an entry whose value is none and a pointer to the lifted value otherwise.
func MapOf[K comparable, V any](v Value) (map[K]V, error) {
	if v == nil {
		return nil, nil
	}

	entries, ok := v.([]Value)
	if !ok {
		var zk K
		var zv V
		return nil, fmt.Errorf("component: expected a map of %T to %T, got %T", zk, zv, v)
	}

	out := make(map[K]V, len(entries))
	keyType := reflect.TypeFor[K]()
	valType := reflect.TypeFor[V]()
	for i, e := range entries {
		pair, ok := e.([]Value)
		if !ok || len(pair) != 2 {
			return nil, fmt.Errorf("component: map entry %d: expected a [key, value] pair, got %#v", i, e)
		}
		key, err := convertMapField[K](pair[0], keyType, i, "key")
		if err != nil {
			return nil, err
		}
		val, err := convertMapField[V](pair[1], valType, i, "value")
		if err != nil {
			return nil, err
		}
		out[key] = val
	}
	return out, nil
}

// convertMapField narrows one map entry's key or value to T, the same way
// ListOf narrows a widened scalar list element. When T is a pointer type it
// additionally unwraps an option<E>: v arrives as nil (none) or the bare
// element value (some, per OptionDesc's doc -- never wrapped), so a nil
// source becomes a nil T and a present one is boxed into a fresh *E.
func convertMapField[T any](v Value, target reflect.Type, index int, which string) (T, error) {
	if t, ok := v.(T); ok {
		return t, nil
	}
	var zero T

	if target.Kind() == reflect.Pointer {
		if v == nil {
			return zero, nil
		}
		elemType := target.Elem()
		ev := reflect.ValueOf(v)
		if !ev.IsValid() || !ev.CanConvert(elemType) {
			return zero, fmt.Errorf("component: map entry %d %s is %T, want %s", index, which, v, target)
		}
		ptr := reflect.New(elemType)
		ptr.Elem().Set(ev.Convert(elemType))
		return ptr.Interface().(T), nil
	}

	if v == nil {
		return zero, fmt.Errorf("component: map entry %d %s is none, want %T", index, which, zero)
	}
	ev := reflect.ValueOf(v)
	if !ev.IsValid() || !ev.CanConvert(target) {
		return zero, fmt.Errorf("component: map entry %d %s is %T, want %T", index, which, v, zero)
	}
	return ev.Convert(target).Interface().(T), nil
}
