package abi

import (
	"testing"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// --- Test Helpers ---

// noResolver is a resolver that doesn't resolve any indices (used for primitives-only tests).
func noResolver(idx uint32) binary.TypeDesc {
	panic("unexpected type index resolution")
}

// mapResolver creates a simple resolver from a map of type indices.
func mapResolver(m map[uint32]binary.TypeDesc) Resolver {
	return func(idx uint32) binary.TypeDesc {
		t, ok := m[idx]
		if !ok {
			panic("type index not found in resolver map")
		}
		return t
	}
}

// --- Primitive Tests ---

func TestAlignmentPrimitives(t *testing.T) {
	tests := []struct {
		name     string
		desc     binary.TypeDesc
		expected uint32
	}{
		{"bool", binary.PrimitiveDesc{Prim: "bool"}, 1},
		{"s8", binary.PrimitiveDesc{Prim: "s8"}, 1},
		{"u8", binary.PrimitiveDesc{Prim: "u8"}, 1},
		{"s16", binary.PrimitiveDesc{Prim: "s16"}, 2},
		{"u16", binary.PrimitiveDesc{Prim: "u16"}, 2},
		{"s32", binary.PrimitiveDesc{Prim: "s32"}, 4},
		{"u32", binary.PrimitiveDesc{Prim: "u32"}, 4},
		{"f32", binary.PrimitiveDesc{Prim: "f32"}, 4},
		{"s64", binary.PrimitiveDesc{Prim: "s64"}, 8},
		{"u64", binary.PrimitiveDesc{Prim: "u64"}, 8},
		{"f64", binary.PrimitiveDesc{Prim: "f64"}, 8},
		{"char", binary.PrimitiveDesc{Prim: "char"}, 4},
		{"string", binary.PrimitiveDesc{Prim: "string"}, 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			align, err := Alignment(tt.desc, noResolver)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if align != tt.expected {
				t.Errorf("got %d, want %d", align, tt.expected)
			}
		})
	}
}

func TestSizePrimitives(t *testing.T) {
	tests := []struct {
		name     string
		desc     binary.TypeDesc
		expected uint32
	}{
		{"bool", binary.PrimitiveDesc{Prim: "bool"}, 1},
		{"s8", binary.PrimitiveDesc{Prim: "s8"}, 1},
		{"u8", binary.PrimitiveDesc{Prim: "u8"}, 1},
		{"s16", binary.PrimitiveDesc{Prim: "s16"}, 2},
		{"u16", binary.PrimitiveDesc{Prim: "u16"}, 2},
		{"s32", binary.PrimitiveDesc{Prim: "s32"}, 4},
		{"u32", binary.PrimitiveDesc{Prim: "u32"}, 4},
		{"f32", binary.PrimitiveDesc{Prim: "f32"}, 4},
		{"s64", binary.PrimitiveDesc{Prim: "s64"}, 8},
		{"u64", binary.PrimitiveDesc{Prim: "u64"}, 8},
		{"f64", binary.PrimitiveDesc{Prim: "f64"}, 8},
		{"char", binary.PrimitiveDesc{Prim: "char"}, 4},
		{"string", binary.PrimitiveDesc{Prim: "string"}, 8},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			size, err := Size(tt.desc, noResolver)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if size != tt.expected {
				t.Errorf("got %d, want %d", size, tt.expected)
			}
		})
	}
}

// --- Align Helper Tests ---

func TestAlign(t *testing.T) {
	tests := []struct {
		offset    uint32
		alignment uint32
		expected  uint32
	}{
		{0, 1, 0},
		{0, 2, 0},
		{0, 4, 0},
		{1, 1, 1},
		{1, 2, 2},
		{1, 4, 4},
		{5, 4, 8},
		{7, 4, 8},
		{8, 4, 8},
		{9, 4, 12},
	}

	for _, tt := range tests {
		got := Align(tt.offset, tt.alignment)
		if got != tt.expected {
			t.Errorf("Align(%d, %d) = %d, want %d", tt.offset, tt.alignment, got, tt.expected)
		}
	}
}

// --- Record Tests ---

func TestRecordSimple(t *testing.T) {
	// record{x: u8, y: u32}
	// x: offset 0, size 1, align 1
	// y: offset align(1, 4) = 4, size 4, align 4
	// total: 4 + 4 = 8, aligned to 4 = 8
	record := binary.RecordDesc{
		Fields: []binary.RecordField{
			{Name: "x", Type: binary.TypeRef{Primitive: "u8"}},
			{Name: "y", Type: binary.TypeRef{Primitive: "u32"}},
		},
	}

	align, err := Alignment(record, noResolver)
	if err != nil {
		t.Fatalf("Alignment: %v", err)
	}
	if align != 4 {
		t.Errorf("Alignment: got %d, want 4", align)
	}

	size, err := Size(record, noResolver)
	if err != nil {
		t.Fatalf("Size: %v", err)
	}
	if size != 8 {
		t.Errorf("Size: got %d, want 8", size)
	}
}

func TestRecordNested(t *testing.T) {
	// record{a: u8, b: u16}
	// a: offset 0, size 1, align 1
	// b: offset align(1, 2) = 2, size 2, align 2
	// total: 2 + 2 = 4, aligned to 2 = 4
	record := binary.RecordDesc{
		Fields: []binary.RecordField{
			{Name: "a", Type: binary.TypeRef{Primitive: "u8"}},
			{Name: "b", Type: binary.TypeRef{Primitive: "u16"}},
		},
	}

	align, err := Alignment(record, noResolver)
	if err != nil {
		t.Fatalf("Alignment: %v", err)
	}
	if align != 2 {
		t.Errorf("Alignment: got %d, want 2", align)
	}

	size, err := Size(record, noResolver)
	if err != nil {
		t.Fatalf("Size: %v", err)
	}
	if size != 4 {
		t.Errorf("Size: got %d, want 4", size)
	}
}

// --- Flags Tests ---

func TestFlagsAlignment(t *testing.T) {
	tests := []struct {
		name      string
		numLabels int
		expected  uint32
	}{
		{"1", 1, 1},
		{"8", 8, 1},
		{"9", 9, 2},
		{"16", 16, 2},
		{"17", 17, 4},
		{"32", 32, 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flags := binary.FlagsDesc{
				Names: make([]string, tt.numLabels),
			}
			for i := range flags.Names {
				flags.Names[i] = string(rune('a' + i))
			}

			align, err := Alignment(flags, noResolver)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if align != tt.expected {
				t.Errorf("got %d, want %d", align, tt.expected)
			}
		})
	}
}

func TestFlagsSize(t *testing.T) {
	tests := []struct {
		name      string
		numLabels int
		expected  uint32
	}{
		{"1", 1, 1},
		{"8", 8, 1},
		{"9", 9, 2},
		{"16", 16, 2},
		{"17", 17, 4},
		{"32", 32, 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flags := binary.FlagsDesc{
				Names: make([]string, tt.numLabels),
			}
			for i := range flags.Names {
				flags.Names[i] = string(rune('a' + i))
			}

			size, err := Size(flags, noResolver)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if size != tt.expected {
				t.Errorf("got %d, want %d", size, tt.expected)
			}
		})
	}
}

// --- Enum Tests ---

// TestEnumAlignment checks the discriminant-width boundaries an enum's
// alignment mirrors DiscriminantType's (u8 up to 256 cases, u16 up to
// 65536, u32 beyond) -- NOT flags' 8/16/32-label tiers (an enum's core
// representation is always a single discriminant value regardless of case
// count, unlike flags' multi-word bitset; see sizeEnumNumCases' doc in
// layout.go). n=37 is wasi:filesystem/types' real `error-code` enum,
// which a buggy flags-shaped bound used to reject outright.
func TestEnumAlignment(t *testing.T) {
	tests := []struct {
		name     string
		numCases int
		expected uint32
	}{
		{"1", 1, 1},
		{"8", 8, 1},
		{"9", 9, 1},
		{"16", 16, 1},
		{"17", 17, 1},
		{"32", 32, 1},
		{"37", 37, 1},
		{"256", 256, 1},
		{"257", 257, 2},
		{"65536", 65536, 2},
		{"65537", 65537, 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enum := binary.EnumDesc{
				Cases: make([]string, tt.numCases),
			}
			for i := range enum.Cases {
				enum.Cases[i] = string(rune('a' + i))
			}

			align, err := Alignment(enum, noResolver)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if align != tt.expected {
				t.Errorf("got %d, want %d", align, tt.expected)
			}
		})
	}
}

// --- Variant Tests ---

func TestVariantSimple(t *testing.T) {
	// variant { a, b: u32 }
	// discriminant: u8 (1 byte, align 1)
	// cases: none, u32 (4 bytes, align 4)
	// size = align(1, 4) + 4 = 4 + 4 = 8
	// aligned to max(1, 4) = 4 -> 8
	variant := binary.VariantDesc{
		Cases: []binary.VariantCase{
			{Name: "a", Type: nil},
			{Name: "b", Type: &binary.TypeRef{Primitive: "u32"}},
		},
	}

	align, err := Alignment(variant, noResolver)
	if err != nil {
		t.Fatalf("Alignment: %v", err)
	}
	if align != 4 {
		t.Errorf("Alignment: got %d, want 4", align)
	}

	size, err := Size(variant, noResolver)
	if err != nil {
		t.Fatalf("Size: %v", err)
	}
	if size != 8 {
		t.Errorf("Size: got %d, want 8", size)
	}
}

// --- Option Tests ---

func TestOptionU32(t *testing.T) {
	// option<u32>
	// This is a variant { none, some: u32 }
	// discriminant: u8 (1 byte), case: u32 (4 bytes, align 4)
	// size = align(1, 4) + 4 = 4 + 4 = 8, aligned to 4 = 8
	option := binary.OptionDesc{
		Element: binary.TypeRef{Primitive: "u32"},
	}

	align, err := Alignment(option, noResolver)
	if err != nil {
		t.Fatalf("Alignment: %v", err)
	}
	if align != 4 {
		t.Errorf("Alignment: got %d, want 4", align)
	}

	size, err := Size(option, noResolver)
	if err != nil {
		t.Fatalf("Size: %v", err)
	}
	if size != 8 {
		t.Errorf("Size: got %d, want 8", size)
	}
}

// --- List Tests (regression for size/alignment bugs) ---

func TestListDynamicSizeAndAlignment(t *testing.T) {
	// A dynamic list is always represented as pointer + length = 8 bytes,
	// with pointer alignment (4), regardless of the element type.
	// (spec: size(list) = 8, alignment(list) = 4.)
	tests := []struct {
		name     string
		elem     binary.TypeRef
		wantSize uint32
		wantAlgn uint32
	}{
		{"list<u64>", binary.TypeRef{Primitive: "u64"}, 8, 4},
		{"list<string>", binary.TypeRef{Primitive: "string"}, 8, 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			list := binary.ListDesc{Element: tt.elem}

			size, err := Size(list, noResolver)
			if err != nil {
				t.Fatalf("Size: %v", err)
			}
			if size != tt.wantSize {
				t.Errorf("Size: got %d, want %d", size, tt.wantSize)
			}

			align, err := Alignment(list, noResolver)
			if err != nil {
				t.Fatalf("Alignment: %v", err)
			}
			if align != tt.wantAlgn {
				t.Errorf("Alignment: got %d, want %d", align, tt.wantAlgn)
			}
		})
	}
}

// --- Discriminant Type Tests ---

func TestDiscriminantType(t *testing.T) {
	tests := []struct {
		numCases int
		expected string
	}{
		{1, "u8"},
		{8, "u8"},
		{256, "u8"},    // log2(256)=8 bits, 8/8=1 byte -> u8
		{257, "u16"},   // log2(257)≈8 bits, >8/8 bytes -> u16
		{65535, "u16"}, // log2(65535)≈16 bits, 16/8=2 bytes -> u16
		{65536, "u16"}, // log2(65536)=16 bits, 16/8=2 bytes -> u16
		{65537, "u32"}, // log2(65537)>16 bits, >16/8 bytes -> u32
		{1000000, "u32"},
	}

	for _, tt := range tests {
		got := DiscriminantType(tt.numCases)
		if got != tt.expected {
			t.Errorf("DiscriminantType(%d) = %q, want %q", tt.numCases, got, tt.expected)
		}
	}
}

// --- Result Tests ---

func TestResultOkU32(t *testing.T) {
	// result<u32>
	// discriminant: u8 (1 byte)
	// ok: u32 (4 bytes, align 4)
	// size = align(1, 4) + 4 = 4 + 4 = 8, aligned to 4 = 8
	result := binary.ResultDesc{
		Ok:  &binary.TypeRef{Primitive: "u32"},
		Err: nil,
	}

	align, err := Alignment(result, noResolver)
	if err != nil {
		t.Fatalf("Alignment: %v", err)
	}
	if align != 4 {
		t.Errorf("Alignment: got %d, want 4", align)
	}

	size, err := Size(result, noResolver)
	if err != nil {
		t.Fatalf("Size: %v", err)
	}
	if size != 8 {
		t.Errorf("Size: got %d, want 8", size)
	}
}

// --- Handle Tests ---

func TestOwnSize(t *testing.T) {
	own := binary.OwnDesc{ResourceType: 0}
	size, err := Size(own, noResolver)
	if err != nil {
		t.Fatalf("Size: %v", err)
	}
	if size != 4 {
		t.Errorf("Size: got %d, want 4", size)
	}

	align, err := Alignment(own, noResolver)
	if err != nil {
		t.Fatalf("Alignment: %v", err)
	}
	if align != 4 {
		t.Errorf("Alignment: got %d, want 4", align)
	}
}

func TestBorrowSize(t *testing.T) {
	borrow := binary.BorrowDesc{ResourceType: 0}
	size, err := Size(borrow, noResolver)
	if err != nil {
		t.Fatalf("Size: %v", err)
	}
	if size != 4 {
		t.Errorf("Size: got %d, want 4", size)
	}

	align, err := Alignment(borrow, noResolver)
	if err != nil {
		t.Fatalf("Alignment: %v", err)
	}
	if align != 4 {
		t.Errorf("Alignment: got %d, want 4", align)
	}
}

// --- Flatten Tests ---

func TestFlattenPrimitives(t *testing.T) {
	tests := []struct {
		name     string
		desc     binary.TypeDesc
		expected []string
	}{
		{"bool", binary.PrimitiveDesc{Prim: "bool"}, []string{"i32"}},
		{"u32", binary.PrimitiveDesc{Prim: "u32"}, []string{"i32"}},
		{"s32", binary.PrimitiveDesc{Prim: "s32"}, []string{"i32"}},
		{"u64", binary.PrimitiveDesc{Prim: "u64"}, []string{"i64"}},
		{"s64", binary.PrimitiveDesc{Prim: "s64"}, []string{"i64"}},
		{"f32", binary.PrimitiveDesc{Prim: "f32"}, []string{"f32"}},
		{"f64", binary.PrimitiveDesc{Prim: "f64"}, []string{"f64"}},
		{"char", binary.PrimitiveDesc{Prim: "char"}, []string{"i32"}},
		{"string", binary.PrimitiveDesc{Prim: "string"}, []string{"i32", "i32"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flat, err := Flatten(tt.desc, noResolver)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(flat) != len(tt.expected) {
				t.Errorf("got len %d, want %d", len(flat), len(tt.expected))
			}
			for i, v := range flat {
				if i >= len(tt.expected) || v != tt.expected[i] {
					t.Errorf("at index %d: got %s, want %s", i, v, tt.expected[i])
				}
			}
		})
	}
}

func TestFlattenRecord(t *testing.T) {
	// record{x: u32, y: u32} -> [i32, i32]
	record := binary.RecordDesc{
		Fields: []binary.RecordField{
			{Name: "x", Type: binary.TypeRef{Primitive: "u32"}},
			{Name: "y", Type: binary.TypeRef{Primitive: "u32"}},
		},
	}

	flat, err := Flatten(record, noResolver)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []string{"i32", "i32"}
	if len(flat) != len(expected) {
		t.Errorf("got len %d, want %d", len(flat), len(expected))
	}
	for i, v := range flat {
		if i >= len(expected) || v != expected[i] {
			t.Errorf("at index %d: got %s, want %s", i, v, expected[i])
		}
	}
}

func TestFlattenOption(t *testing.T) {
	// option<u32> -> [i32, i32] (discriminant + u32)
	option := binary.OptionDesc{
		Element: binary.TypeRef{Primitive: "u32"},
	}

	flat, err := Flatten(option, noResolver)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []string{"i32", "i32"}
	if len(flat) != len(expected) {
		t.Errorf("got len %d, want %d", len(flat), len(expected))
	}
	for i, v := range flat {
		if i >= len(expected) || v != expected[i] {
			t.Errorf("at index %d: got %s, want %s", i, v, expected[i])
		}
	}
}

func TestFlattenFlags(t *testing.T) {
	flags := binary.FlagsDesc{
		Names: []string{"a", "b", "c"},
	}

	flat, err := Flatten(flags, noResolver)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []string{"i32"}
	if len(flat) != len(expected) {
		t.Errorf("got len %d, want %d", len(flat), len(expected))
	}
	for i, v := range flat {
		if i >= len(expected) || v != expected[i] {
			t.Errorf("at index %d: got %s, want %s", i, v, expected[i])
		}
	}
}

func TestFlattenEnum(t *testing.T) {
	enum := binary.EnumDesc{
		Cases: []string{"a", "b", "c"},
	}

	flat, err := Flatten(enum, noResolver)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []string{"i32"}
	if len(flat) != len(expected) {
		t.Errorf("got len %d, want %d", len(flat), len(expected))
	}
	for i, v := range flat {
		if i >= len(expected) || v != expected[i] {
			t.Errorf("at index %d: got %s, want %s", i, v, expected[i])
		}
	}
}

// --- FlattenFunc Tests ---

func TestFlattenFuncSimple(t *testing.T) {
	// func(u32, u32) -> u32
	// Params: [i32, i32]
	// Results: [i32]
	// No spill needed
	fn := binary.FuncDesc{
		Params: []binary.FuncParam{
			{Name: "a", Type: binary.TypeRef{Primitive: "u32"}},
			{Name: "b", Type: binary.TypeRef{Primitive: "u32"}},
		},
		Results: binary.FuncResults{
			Unnamed: &binary.TypeRef{Primitive: "u32"},
		},
	}

	params, results, err := FlattenFunc(fn, noResolver, "lift")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(params) != 2 || params[0] != "i32" || params[1] != "i32" {
		t.Errorf("params: got %v, want [i32, i32]", params)
	}
	if len(results) != 1 || results[0] != "i32" {
		t.Errorf("results: got %v, want [i32]", results)
	}
}

func TestFlattenFuncManyParams(t *testing.T) {
	// func(u32, u32, ..., u32) -> u32 with 17 params
	// Params should spill to single i32 pointer
	params := make([]binary.FuncParam, 17)
	for i := range 17 {
		params[i] = binary.FuncParam{
			Name: string(rune('a' + i)),
			Type: binary.TypeRef{Primitive: "u32"},
		}
	}

	fn := binary.FuncDesc{
		Params: params,
		Results: binary.FuncResults{
			Unnamed: &binary.TypeRef{Primitive: "u32"},
		},
	}

	flatParams, flatResults, err := FlattenFunc(fn, noResolver, "lift")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should spill to single pointer
	if len(flatParams) != 1 || flatParams[0] != "i32" {
		t.Errorf("params: got %v, want [i32]", flatParams)
	}
	if len(flatResults) != 1 || flatResults[0] != "i32" {
		t.Errorf("results: got %v, want [i32]", flatResults)
	}
}

func TestFlattenFuncManyResults(t *testing.T) {
	// func(u32) -> (u32, u32) with 2 results in lift mode
	// Results should spill to pointer return
	fn := binary.FuncDesc{
		Params: []binary.FuncParam{
			{Name: "a", Type: binary.TypeRef{Primitive: "u32"}},
		},
		Results: binary.FuncResults{
			Named: []binary.FuncResult{
				{Name: "x", Type: binary.TypeRef{Primitive: "u32"}},
				{Name: "y", Type: binary.TypeRef{Primitive: "u32"}},
			},
		},
	}

	flatParams, flatResults, err := FlattenFunc(fn, noResolver, "lift")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should return single pointer
	if len(flatParams) != 1 || flatParams[0] != "i32" {
		t.Errorf("params: got %v, want [i32]", flatParams)
	}
	if len(flatResults) != 1 || flatResults[0] != "i32" {
		t.Errorf("results: got %v, want [i32]", flatResults)
	}
}

func TestFlattenFuncManyResultsLower(t *testing.T) {
	// func(u32) -> (u32, u32) with 2 results in lower mode
	// Results should spill to output pointer in params
	fn := binary.FuncDesc{
		Params: []binary.FuncParam{
			{Name: "a", Type: binary.TypeRef{Primitive: "u32"}},
		},
		Results: binary.FuncResults{
			Named: []binary.FuncResult{
				{Name: "x", Type: binary.TypeRef{Primitive: "u32"}},
				{Name: "y", Type: binary.TypeRef{Primitive: "u32"}},
			},
		},
	}

	flatParams, flatResults, err := FlattenFunc(fn, noResolver, "lower")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should pass output pointer in params
	if len(flatParams) != 2 || flatParams[0] != "i32" || flatParams[1] != "i32" {
		t.Errorf("params: got %v, want [i32, i32]", flatParams)
	}
	if len(flatResults) != 0 {
		t.Errorf("results: got %v, want []", flatResults)
	}
}

// --- Error Tests ---

func TestUnknownPrimitive(t *testing.T) {
	desc := binary.PrimitiveDesc{Prim: "unknown"}
	_, err := Alignment(desc, noResolver)
	if err == nil {
		t.Error("expected error for unknown primitive")
	}
}

func TestUnsupportedType(t *testing.T) {
	desc := binary.FuncDesc{}
	_, err := Alignment(desc, noResolver)
	if err == nil {
		t.Error("expected error for unsupported type (func)")
	}
}
