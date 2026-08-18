package component_test

import (
	"math"
	"testing"

	"github.com/samyfodil/wazy/component"
)

// ListOf exists so a host function can read a list without caring which of the
// two shapes it arrived in: the typed slice a scalar list lifts to, or the
// []Value everything else lifts to and older host functions still hand over.

func TestListOfTypedShape(t *testing.T) {
	in := []uint32{1, 2, 3}

	got, err := component.ListOf[uint32](component.Value(in))
	if err != nil {
		t.Fatalf("ListOf: %v", err)
	}
	if len(got) != 3 || got[0] != 1 || got[2] != 3 {
		t.Fatalf("got %v, want [1 2 3]", got)
	}

	// The typed shape is returned as it arrived. A copy would make the fast
	// path quietly as expensive as the slow one.
	if &got[0] != &in[0] {
		t.Error("the typed slice was copied; it should be returned as-is")
	}
}

func TestListOfValueShape(t *testing.T) {
	// What a host function written before typed lists hands over, and what
	// lifting still produces for a list of anything but a fixed-width scalar.
	vals := []component.Value{uint32(1), uint32(2), uint32(3)}

	got, err := component.ListOf[uint32](vals)
	if err != nil {
		t.Fatalf("ListOf: %v", err)
	}
	if len(got) != 3 || got[1] != 2 {
		t.Fatalf("got %v, want [1 2 3]", got)
	}
}

// A scalar element arrives widened -- a u16 as uint32, an s8 as int32 -- so
// asking for the narrow type has to convert rather than fail.
func TestListOfNarrowsWidenedScalars(t *testing.T) {
	t.Run("u16 from uint32", func(t *testing.T) {
		got, err := component.ListOf[uint16]([]component.Value{uint32(1), uint32(0xFFFF)})
		if err != nil {
			t.Fatalf("ListOf: %v", err)
		}
		if got[1] != 0xFFFF {
			t.Errorf("got %v, want [1 65535]", got)
		}
	})

	t.Run("s8 from int32", func(t *testing.T) {
		got, err := component.ListOf[int8]([]component.Value{int32(-128), int32(127)})
		if err != nil {
			t.Fatalf("ListOf: %v", err)
		}
		if got[0] != -128 || got[1] != 127 {
			t.Errorf("got %v, want [-128 127]", got)
		}
	})
}

// The case with no typed shape at all: a list of strings is always []Value, and
// walking it is exactly what this saves a caller from writing.
func TestListOfStrings(t *testing.T) {
	got, err := component.ListOf[string]([]component.Value{"a", "b"})
	if err != nil {
		t.Fatalf("ListOf: %v", err)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("got %v, want [a b]", got)
	}
}

func TestListOfFloatsAndBools(t *testing.T) {
	fs, err := component.ListOf[float64]([]component.Value{1.5, math.Inf(1)})
	if err != nil {
		t.Fatalf("floats: %v", err)
	}
	if fs[0] != 1.5 || !math.IsInf(fs[1], 1) {
		t.Errorf("got %v", fs)
	}

	bs, err := component.ListOf[bool]([]component.Value{true, false})
	if err != nil {
		t.Fatalf("bools: %v", err)
	}
	if !bs[0] || bs[1] {
		t.Errorf("got %v, want [true false]", bs)
	}
}

func TestListOfEmpty(t *testing.T) {
	got, err := component.ListOf[uint32]([]component.Value{})
	if err != nil {
		t.Fatalf("ListOf: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want an empty list", got)
	}
}

// Failures name what was wrong and where, since a list of the wrong thing is a
// mistake in the host function's idea of the WIT, not a runtime condition.
func TestListOfErrors(t *testing.T) {
	t.Run("not a list", func(t *testing.T) {
		if _, err := component.ListOf[uint32](component.Value("nope")); err == nil {
			t.Error("expected an error for a non-list value")
		}
	})

	t.Run("element of an unconvertible type", func(t *testing.T) {
		_, err := component.ListOf[uint32]([]component.Value{uint32(1), "two"})
		if err == nil {
			t.Fatal("expected an error for a string element")
		}
		if want := "element 1"; !contains(err.Error(), want) {
			t.Errorf("error %q should name the offending index (%q)", err, want)
		}
	})

	t.Run("bool is not a number", func(t *testing.T) {
		if _, err := component.ListOf[uint32]([]component.Value{true}); err == nil {
			t.Error("expected an error converting bool to uint32")
		}
	})
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
