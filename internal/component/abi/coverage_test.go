package abi

// This file exercises the fail-loud error branches of the layout/flatten code
// so that every defensive error return has a triggering test. It complements
// the value-correctness coverage in layout_test.go / flatten_test.go and the
// differential oracle in oracle_test.go.

import (
	"strings"
	"testing"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// badRef is an empty TypeRef: neither a primitive name nor a type index. Any
// attempt to resolve it via resolveType returns the "neither primitive nor
// index" error, which lets us drive the error-propagation branches of every
// composite type from the outside.
var badRef = binary.TypeRef{}

// --- Align zero-alignment branch ---

func TestAlignZeroAlignment(t *testing.T) {
	// alignment == 0 must return the offset unchanged (avoids div-by-zero).
	if got := Align(7, 0); got != 7 {
		t.Errorf("Align(7, 0) = %d, want 7", got)
	}
}

// --- Unsupported top-level type descriptors ---

func TestUnsupportedTypesSizeAlignmentFlatten(t *testing.T) {
	unsupported := []struct {
		name string
		desc binary.TypeDesc
	}{
		{"func", binary.FuncDesc{}},
		{"instance", binary.InstanceDesc{}},
		{"component", binary.ComponentDesc{}},
		{"resource", binary.ResourceDesc{}},
	}

	for _, u := range unsupported {
		t.Run(u.name, func(t *testing.T) {
			if _, err := Size(u.desc, noResolver); err == nil {
				t.Error("Size: expected error, got nil")
			}
			if _, err := Alignment(u.desc, noResolver); err == nil {
				t.Error("Alignment: expected error, got nil")
			}
			if _, err := Flatten(u.desc, noResolver); err == nil {
				t.Error("Flatten: expected error, got nil")
			}
		})
	}
}

// --- Unknown primitive strings ---

func TestUnknownPrimitiveAllPaths(t *testing.T) {
	desc := binary.PrimitiveDesc{Prim: "not-a-real-primitive"}

	if _, err := Size(desc, noResolver); err == nil {
		t.Error("Size: expected error for unknown primitive")
	}
	if _, err := Alignment(desc, noResolver); err == nil {
		t.Error("Alignment: expected error for unknown primitive")
	}
	if _, err := Flatten(desc, noResolver); err == nil {
		t.Error("Flatten: expected error for unknown primitive")
	}
}

// --- resolveType error branches (via the public entry points) ---

func TestResolveTypeNeitherPrimitiveNorIndex(t *testing.T) {
	// A record field holding an empty TypeRef drives resolveType's
	// "neither primitive nor index" branch through sizeRecord /
	// alignmentRecord / flattenRecord.
	record := binary.RecordDesc{
		Fields: []binary.RecordField{{Name: "x", Type: badRef}},
	}
	if _, err := Size(record, noResolver); err == nil {
		t.Error("Size: expected error for empty type ref")
	}
	if _, err := Alignment(record, noResolver); err == nil {
		t.Error("Alignment: expected error for empty type ref")
	}
	if _, err := Flatten(record, noResolver); err == nil {
		t.Error("Flatten: expected error for empty type ref")
	}
}

func TestResolveTypeIndexRequiresResolver(t *testing.T) {
	// A type index with a nil resolver drives the "type index requires
	// resolver" branch of resolveType.
	idx := uint32(0)
	record := binary.RecordDesc{
		Fields: []binary.RecordField{{Name: "x", Type: binary.TypeRef{TypeIndex: &idx}}},
	}
	_, err := Size(record, nil)
	if err == nil {
		t.Fatal("Size: expected error for type index with nil resolver")
	}
	if !strings.Contains(err.Error(), "requires resolver") {
		t.Errorf("Size: got %q, want error mentioning resolver", err)
	}
	if _, err := Alignment(record, nil); err == nil {
		t.Error("Alignment: expected error for type index with nil resolver")
	}
	if _, err := Flatten(record, nil); err == nil {
		t.Error("Flatten: expected error for type index with nil resolver")
	}
}

// --- Error propagation through every composite kind ---
//
// Each composite embeds a badRef payload so resolveType fails, and we assert
// the error surfaces from Size, Alignment, and Flatten.

func TestErrorPropagationComposites(t *testing.T) {
	idxBad := uint32(0)
	tests := []struct {
		name string
		desc binary.TypeDesc
	}{
		{"record", binary.RecordDesc{Fields: []binary.RecordField{{Name: "f", Type: badRef}}}},
		{"tuple", binary.TupleDesc{Elements: []binary.TypeRef{badRef}}},
		{"list", binary.ListDesc{Element: badRef}},
		{"option", binary.OptionDesc{Element: badRef}},
		{"variant", binary.VariantDesc{Cases: []binary.VariantCase{{Name: "c", Type: &badRef}}}},
		{"result_ok", binary.ResultDesc{Ok: &badRef}},
		{"result_err", binary.ResultDesc{Err: &badRef}},
		// A type-index payload with the error resolver (nil resolver path):
		{"list_needs_resolver", binary.ListDesc{Element: binary.TypeRef{TypeIndex: &idxBad}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// list is special: its Size/Alignment/Flatten no longer inspect
			// the element (dynamic list is fixed 8/4/[i32,i32]), so those must
			// NOT error. We only assert error for the non-list composites here.
			isList := tt.name == "list" || tt.name == "list_needs_resolver"

			_, sErr := Size(tt.desc, nil)
			_, aErr := Alignment(tt.desc, nil)
			_, fErr := Flatten(tt.desc, nil)

			if isList {
				if sErr != nil || aErr != nil || fErr != nil {
					t.Errorf("list: expected no error (fixed layout), got size=%v align=%v flat=%v", sErr, aErr, fErr)
				}
				return
			}
			if sErr == nil {
				t.Error("Size: expected error")
			}
			if aErr == nil {
				t.Error("Alignment: expected error")
			}
			if fErr == nil {
				t.Error("Flatten: expected error")
			}
		})
	}
}

// bogusRef resolves successfully (it names a "primitive") but that primitive
// is unknown, so the subsequent Size/Alignment/Flatten of the resolved type
// fails. This drives the SECOND error branch of each composite (payload
// resolved, then its own layout computation errors), distinct from badRef
// which fails in resolveType itself.
var bogusRef = binary.TypeRef{Primitive: "not-a-real-primitive"}

func TestErrorPropagationResolvedButBadPayload(t *testing.T) {
	tests := []struct {
		name string
		desc binary.TypeDesc
	}{
		{"record", binary.RecordDesc{Fields: []binary.RecordField{{Name: "f", Type: bogusRef}}}},
		{"tuple", binary.TupleDesc{Elements: []binary.TypeRef{bogusRef}}},
		{"option", binary.OptionDesc{Element: bogusRef}},
		{"variant", binary.VariantDesc{Cases: []binary.VariantCase{{Name: "c", Type: &bogusRef}}}},
		{"result_ok", binary.ResultDesc{Ok: &bogusRef}},
		{"result_err", binary.ResultDesc{Err: &bogusRef}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Size(tt.desc, noResolver); err == nil {
				t.Error("Size: expected error")
			}
			if _, err := Alignment(tt.desc, noResolver); err == nil {
				t.Error("Alignment: expected error")
			}
			if _, err := Flatten(tt.desc, noResolver); err == nil {
				t.Error("Flatten: expected error")
			}
		})
	}
}

// TestErrorPropagationResultBothPresent covers the Err branch of result when
// Ok is valid (so the first branch passes and the second one errors), for
// Size, Alignment, and Flatten.
func TestErrorPropagationResultErrAfterOk(t *testing.T) {
	desc := binary.ResultDesc{
		Ok:  &binary.TypeRef{Primitive: "u32"},
		Err: &badRef,
	}
	if _, err := Size(desc, noResolver); err == nil {
		t.Error("Size: expected error from err payload")
	}
	if _, err := Alignment(desc, noResolver); err == nil {
		t.Error("Alignment: expected error from err payload")
	}
	if _, err := Flatten(desc, noResolver); err == nil {
		t.Error("Flatten: expected error from err payload")
	}
}

// TestVariantSecondCasePayloadError ensures the loop over variant cases keeps
// checking payloads past the first: first case is valid, second errors.
func TestVariantSecondCasePayloadError(t *testing.T) {
	desc := binary.VariantDesc{
		Cases: []binary.VariantCase{
			{Name: "ok", Type: &binary.TypeRef{Primitive: "u32"}},
			{Name: "bad", Type: &badRef},
		},
	}
	if _, err := Size(desc, noResolver); err == nil {
		t.Error("Size: expected error from second case payload")
	}
	if _, err := Alignment(desc, noResolver); err == nil {
		t.Error("Alignment: expected error from second case payload")
	}
	if _, err := Flatten(desc, noResolver); err == nil {
		t.Error("Flatten: expected error from second case payload")
	}
}

// --- Invalid flags/enum label counts (fail-loud, spec-capped) ---

func TestInvalidFlagsCounts(t *testing.T) {
	for _, n := range []int{0, 33, 64} {
		flags := binary.FlagsDesc{Names: make([]string, n)}
		if _, err := Size(flags, noResolver); err == nil {
			t.Errorf("Size(flags n=%d): expected error", n)
		}
		if _, err := Alignment(flags, noResolver); err == nil {
			t.Errorf("Alignment(flags n=%d): expected error", n)
		}
		if _, err := Flatten(flags, noResolver); err == nil {
			t.Errorf("Flatten(flags n=%d): expected error", n)
		}
	}
}

func TestInvalidEnumCounts(t *testing.T) {
	for _, n := range []int{0, 33} {
		enum := binary.EnumDesc{Cases: make([]string, n)}
		if _, err := Size(enum, noResolver); err == nil {
			t.Errorf("Size(enum n=%d): expected error", n)
		}
		if _, err := Alignment(enum, noResolver); err == nil {
			t.Errorf("Alignment(enum n=%d): expected error", n)
		}
		if _, err := Flatten(enum, noResolver); err == nil {
			t.Errorf("Flatten(enum n=%d): expected error", n)
		}
	}
}

// --- DiscriminantType invalid input ---

func TestDiscriminantTypeInvalid(t *testing.T) {
	if got := DiscriminantType(0); got != "" {
		t.Errorf("DiscriminantType(0) = %q, want \"\" (invalid)", got)
	}
	if got := DiscriminantType(-5); got != "" {
		t.Errorf("DiscriminantType(-5) = %q, want \"\" (invalid)", got)
	}
}

// --- FlattenFunc error branches ---

func TestFlattenFuncParamError(t *testing.T) {
	// badRef fails in resolveType; bogusRef resolves then fails in Flatten.
	// Both param error branches must surface.
	for _, ref := range []binary.TypeRef{badRef, bogusRef} {
		fn := binary.FuncDesc{
			Params: []binary.FuncParam{{Name: "a", Type: ref}},
			Results: binary.FuncResults{
				Unnamed: &binary.TypeRef{Primitive: "u32"},
			},
		}
		if _, _, err := FlattenFunc(fn, noResolver, "lift"); err == nil {
			t.Errorf("param %+v: expected error", ref)
		}
	}
}

func TestFlattenFuncUnnamedResultError(t *testing.T) {
	for _, ref := range []binary.TypeRef{badRef, bogusRef} {
		r := ref
		fn := binary.FuncDesc{
			Params:  []binary.FuncParam{{Name: "a", Type: binary.TypeRef{Primitive: "u32"}}},
			Results: binary.FuncResults{Unnamed: &r},
		}
		if _, _, err := FlattenFunc(fn, noResolver, "lift"); err == nil {
			t.Errorf("unnamed result %+v: expected error", ref)
		}
	}
}

func TestFlattenFuncNamedResultError(t *testing.T) {
	for _, ref := range []binary.TypeRef{badRef, bogusRef} {
		fn := binary.FuncDesc{
			Params: []binary.FuncParam{{Name: "a", Type: binary.TypeRef{Primitive: "u32"}}},
			Results: binary.FuncResults{
				Named: []binary.FuncResult{{Name: "x", Type: ref}},
			},
		}
		if _, _, err := FlattenFunc(fn, noResolver, "lift"); err == nil {
			t.Errorf("named result %+v: expected error", ref)
		}
	}
}

func TestFlattenFuncInvalidContext(t *testing.T) {
	// Two results (> MAX_FLAT_RESULTS) with an unrecognized context must
	// return the "invalid context" error.
	fn := binary.FuncDesc{
		Params: []binary.FuncParam{{Name: "a", Type: binary.TypeRef{Primitive: "u32"}}},
		Results: binary.FuncResults{
			Named: []binary.FuncResult{
				{Name: "x", Type: binary.TypeRef{Primitive: "u32"}},
				{Name: "y", Type: binary.TypeRef{Primitive: "u32"}},
			},
		},
	}
	_, _, err := FlattenFunc(fn, noResolver, "bogus-context")
	if err == nil {
		t.Fatal("expected error for invalid context")
	}
	if !strings.Contains(err.Error(), "invalid context") {
		t.Errorf("got %q, want error mentioning invalid context", err)
	}
}

// --- Handle flatten (own/borrow) via the Flatten switch ---

func TestFlattenHandles(t *testing.T) {
	for _, d := range []binary.TypeDesc{
		binary.OwnDesc{ResourceType: 0},
		binary.BorrowDesc{ResourceType: 0},
	} {
		flat, err := Flatten(d, noResolver)
		if err != nil {
			t.Fatalf("Flatten(%T): %v", d, err)
		}
		if len(flat) != 1 || flat[0] != "i32" {
			t.Errorf("Flatten(%T) = %v, want [i32]", d, flat)
		}
	}
}

// --- Discriminant-alignment dominant paths (payload align <= disc align) ---

// TestAlignmentVariantDiscriminantDominates covers alignmentVariant's
// "return discAlign" tail: when no case payload is wider than the u8
// discriminant (all-void cases), the variant alignment is the discriminant's.
func TestAlignmentVariantDiscriminantDominates(t *testing.T) {
	variant := binary.VariantDesc{
		Cases: []binary.VariantCase{
			{Name: "a", Type: nil},
			{Name: "b", Type: nil},
		},
	}
	align, err := Alignment(variant, noResolver)
	if err != nil {
		t.Fatalf("Alignment: %v", err)
	}
	if align != 1 {
		t.Errorf("Alignment = %d, want 1 (discriminant dominates)", align)
	}
}

// TestAlignmentOptionDiscriminantDominates covers alignmentOption's
// "return discAlign" tail: option<u8> has element alignment 1, equal to the
// discriminant, so the discriminant path is taken.
func TestAlignmentOptionDiscriminantDominates(t *testing.T) {
	option := binary.OptionDesc{Element: binary.TypeRef{Primitive: "u8"}}
	align, err := Alignment(option, noResolver)
	if err != nil {
		t.Fatalf("Alignment: %v", err)
	}
	if align != 1 {
		t.Errorf("Alignment = %d, want 1 (discriminant dominates)", align)
	}
}

// --- Direct resolveType branch coverage (same-package test) ---

func TestResolveTypeDirect(t *testing.T) {
	// nil ref
	if _, err := resolveType(nil, noResolver); err == nil {
		t.Error("resolveType(nil): expected error")
	}
	// neither primitive nor index
	if _, err := resolveType(&badRef, noResolver); err == nil {
		t.Error("resolveType(empty): expected error")
	}
	// primitive success
	desc, err := resolveType(&binary.TypeRef{Primitive: "u32"}, noResolver)
	if err != nil {
		t.Fatalf("resolveType(u32): %v", err)
	}
	if p, ok := desc.(binary.PrimitiveDesc); !ok || p.Prim != "u32" {
		t.Errorf("resolveType(u32) = %#v, want PrimitiveDesc{u32}", desc)
	}
}

// --- resolveType via resolver success path with a nested type index ---
// (exercises resolveType's TypeIndex-with-resolver branch alongside the
// non-error record path.)

func TestResolveTypeIndexSuccess(t *testing.T) {
	inner := uint32(0)
	resolve := mapResolver(map[uint32]binary.TypeDesc{
		inner: binary.PrimitiveDesc{Prim: "u64"},
	})
	record := binary.RecordDesc{
		Fields: []binary.RecordField{{Name: "x", Type: binary.TypeRef{TypeIndex: &inner}}},
	}
	size, err := Size(record, resolve)
	if err != nil {
		t.Fatalf("Size: %v", err)
	}
	if size != 8 {
		t.Errorf("Size = %d, want 8", size)
	}
}
