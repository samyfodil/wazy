package abi

import (
	"testing"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// --- joinCoreTypes regression tests ---
//
// Spec (join, applied over a set of candidates observed at a given flat
// position): if all candidates are identical, the join is that type;
// otherwise, if every candidate is in {i32, f32}, the join is i32;
// otherwise the join is i64. Crucially, an i64/f64 candidate anywhere in
// the set forces i64 even if i32/f32 also appear.

func TestJoinCoreTypes(t *testing.T) {
	tests := []struct {
		name     string
		types    []string
		expected string
	}{
		{"empty", []string{}, "i32"}, // defensive fallback, shouldn't occur in practice
		{"single", []string{"i32"}, "i32"},
		{"all same i32", []string{"i32", "i32"}, "i32"},
		{"all same f32", []string{"f32", "f32"}, "f32"},
		{"i32/f32 -> i32", []string{"i32", "f32"}, "i32"},
		{"f32/i32 -> i32", []string{"f32", "i32"}, "i32"},
		// Regression: an f64 (or i64) present alongside i32/f32 must force i64,
		// even though i32 AND f32 both appear in the set.
		{"i32/f32/f64 -> i64", []string{"i32", "f32", "f64"}, "i64"},
		{"i32/f32/i64 -> i64", []string{"i32", "f32", "i64"}, "i64"},
		{"i64/f64 -> i64", []string{"i64", "f64"}, "i64"},
		{"i32/i64 -> i64", []string{"i32", "i64"}, "i64"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := joinCoreTypes(tt.types)
			if got != tt.expected {
				t.Errorf("joinCoreTypes(%v) = %q, want %q", tt.types, got, tt.expected)
			}
		})
	}
}

// TestFlattenVariantJoinMixedFloat is a regression test for a variant whose
// cases at flat position 0 are u32, f32, and f64. The old (buggy) join logic
// returned i32 whenever i32 and f32 both appeared, even in the presence of a
// wider type like f64. The correct join over {i32, f32, f64} is i64.
func TestFlattenVariantJoinMixedFloat(t *testing.T) {
	variant := binary.VariantDesc{
		Cases: []binary.VariantCase{
			{Name: "a", Type: &binary.TypeRef{Primitive: "u32"}},
			{Name: "b", Type: &binary.TypeRef{Primitive: "f32"}},
			{Name: "c", Type: &binary.TypeRef{Primitive: "f64"}},
		},
	}

	flat, err := Flatten(variant, noResolver)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// discriminant (u8 -> i32) + joined position 0 (u32/f32/f64 -> i64)
	expected := []string{"i32", "i64"}
	if len(flat) != len(expected) {
		t.Fatalf("got len %d (%v), want %d (%v)", len(flat), flat, len(expected), expected)
	}
	for i, v := range flat {
		if v != expected[i] {
			t.Errorf("at index %d: got %s, want %s", i, v, expected[i])
		}
	}
}

// TestFlattenVariantJoinI32F32 is the companion case: cases u32/f32 only
// (no wider type present) must join to i32.
func TestFlattenVariantJoinI32F32(t *testing.T) {
	variant := binary.VariantDesc{
		Cases: []binary.VariantCase{
			{Name: "a", Type: &binary.TypeRef{Primitive: "u32"}},
			{Name: "b", Type: &binary.TypeRef{Primitive: "f32"}},
		},
	}

	flat, err := Flatten(variant, noResolver)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []string{"i32", "i32"}
	if len(flat) != len(expected) {
		t.Fatalf("got len %d (%v), want %d (%v)", len(flat), flat, len(expected), expected)
	}
	for i, v := range flat {
		if v != expected[i] {
			t.Errorf("at index %d: got %s, want %s", i, v, expected[i])
		}
	}
}

// TestFlattenVariantJoinViaResolver exercises the same mixed-float join but
// through the Resolver path (variant cases referencing type indices), using
// mapResolver to ensure the resolver plumbing and joinCoreTypes agree.
func TestFlattenVariantJoinViaResolver(t *testing.T) {
	idxU32, idxF32, idxF64 := uint32(0), uint32(1), uint32(2)
	resolve := mapResolver(map[uint32]binary.TypeDesc{
		idxU32: binary.PrimitiveDesc{Prim: "u32"},
		idxF32: binary.PrimitiveDesc{Prim: "f32"},
		idxF64: binary.PrimitiveDesc{Prim: "f64"},
	})

	variant := binary.VariantDesc{
		Cases: []binary.VariantCase{
			{Name: "a", Type: &binary.TypeRef{TypeIndex: &idxU32}},
			{Name: "b", Type: &binary.TypeRef{TypeIndex: &idxF32}},
			{Name: "c", Type: &binary.TypeRef{TypeIndex: &idxF64}},
		},
	}

	flat, err := Flatten(variant, resolve)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []string{"i32", "i64"}
	if len(flat) != len(expected) {
		t.Fatalf("got len %d (%v), want %d (%v)", len(flat), flat, len(expected), expected)
	}
	for i, v := range flat {
		if v != expected[i] {
			t.Errorf("at index %d: got %s, want %s", i, v, expected[i])
		}
	}
}
