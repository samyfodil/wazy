package abi

import (
	"fmt"
	"testing"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// TestLiftFlatKindsMatchesLiftFlat proves LiftFlatKinds (the planned
// guest->host entry: precomputed flat kinds + raw stack bits) lifts exactly
// what LiftFlat lifts from the equivalent []CoreValue, across the whole
// oracle battery -- including the spilled arm, where both expect a single
// pointer core value.
func TestLiftFlatKindsMatchesLiftFlat(t *testing.T) {
	goldenEntries := loadFlatOracleGolden(t)
	battery := loadFlatOracleBattery(t)

	for _, entry := range goldenEntries {
		desc, ok := battery.descByName[entry.Type]
		if !ok {
			continue
		}
		flat, err := Flatten(desc, battery.resolve)
		if err != nil {
			continue
		}
		t.Run(entry.Name, func(t *testing.T) {
			rawValue := battery.valueMap[fmt.Sprintf("%s:%s", entry.Type, entry.Name)]
			v, err := convertTestValue(rawValue, desc, battery.resolve)
			if err != nil {
				t.Fatalf("convert: %v", err)
			}
			// Lower to the flat core values a caller would find on the core
			// stack (a spilling type lowers to its single pointer, exactly
			// what the guest passes).
			mem := make([]byte, 65536)
			core, err := LowerFlat(v, desc, battery.resolve, ReallocFunc(newBumpAllocator(1024).realloc), mem)
			if err != nil {
				t.Fatalf("LowerFlat: %v", err)
			}

			refVal, refErr := LiftFlat(core, desc, battery.resolve, mem)

			bits := make([]uint64, len(core))
			for i, cv := range core {
				bits[i] = cv.Bits
			}
			kindsVal, kindsErr := LiftFlatKinds(flat, bits, desc, battery.resolve, mem)

			if (refErr == nil) != (kindsErr == nil) {
				t.Fatalf("error mismatch: LiftFlat err=%v, LiftFlatKinds err=%v", refErr, kindsErr)
			}
			if refErr != nil {
				return
			}
			if fmt.Sprintf("%#v", kindsVal) != fmt.Sprintf("%#v", refVal) {
				t.Errorf("lift mismatch: kinds %#v, ref %#v", kindsVal, refVal)
			}
		})
	}
}

// TestLiftFlatKindsBitCountMismatch proves the non-spilled arm fails loud
// when the caller's bits don't match the flat kind list -- the guard that
// replaces liftHostArgsPlanned's old per-value stack-underflow check.
func TestLiftFlatKindsBitCountMismatch(t *testing.T) {
	desc := binary.PrimitiveDesc{Prim: "u32"}
	_, err := LiftFlatKinds([]string{"i32"}, nil, desc, nil, nil)
	if err == nil {
		t.Fatal("expected an error for 0 bits against 1 flat kind")
	}
	_, err = LiftFlatKinds([]string{"i32"}, []uint64{1, 2}, desc, nil, nil)
	if err == nil {
		t.Fatal("expected an error for 2 bits against 1 flat kind")
	}
}

// TestLiftFlatKindsSpilledWrongBitCount proves the spilled arm (flat wider
// than MaxFlatParams) requires exactly the one pointer core value, matching
// LiftFlat's own spilled-arity check.
func TestLiftFlatKindsSpilledWrongBitCount(t *testing.T) {
	// 17 u32 fields flatten to 17 > MaxFlatParams entries.
	fields := make([]binary.RecordField, MaxFlatParams+1)
	for i := range fields {
		fields[i] = binary.RecordField{Name: fmt.Sprintf("f%d", i), Type: binary.TypeRef{Primitive: "u32"}}
	}
	desc := binary.RecordDesc{Fields: fields}
	flat, err := Flatten(desc, nil)
	if err != nil {
		t.Fatalf("Flatten: %v", err)
	}
	if len(flat) <= MaxFlatParams {
		t.Fatalf("test type does not spill (width %d)", len(flat))
	}
	if _, err := LiftFlatKinds(flat, []uint64{0, 0}, desc, nil, make([]byte, 64)); err == nil {
		t.Fatal("expected an error for 2 bits against a spilled type")
	}
}

// TestStackValueIterExhausted proves the pooled kinds+bits iterator fails
// loud past its end (a defensive branch: LiftFlatKinds pre-checks the
// lengths, so only a flat-width bug could reach it) and reports Done
// correctly.
func TestStackValueIterExhausted(t *testing.T) {
	it := getStackValueIter([]string{"i32"}, []uint64{7})
	defer putStackValueIter(it)
	if it.Done() {
		t.Fatal("Done before any Next")
	}
	cv, err := it.Next()
	if err != nil || cv.Kind != "i32" || cv.Bits != 7 {
		t.Fatalf("Next: got {%s %d}, err %v", cv.Kind, cv.Bits, err)
	}
	if !it.Done() {
		t.Fatal("not Done after consuming the only value")
	}
	if _, err := it.Next(); err == nil {
		t.Fatal("expected an error reading past the end")
	}
}

// TestLiftListU8CompactShape pins the compact list<u8> lift: the value is a
// []byte holding a COPY of the guest bytes (a lifted value must not alias
// live guest memory), through both the flat path (LiftFlat) and the
// in-memory path (Load), while a non-u8 list keeps the general []Value
// shape.
func TestLiftListU8CompactShape(t *testing.T) {
	mem := make([]byte, 64)
	copy(mem[8:], "abc")

	u8List := binary.ListDesc{Element: binary.TypeRef{Primitive: "u8"}}
	core := []CoreValue{NewCoreValueI32(8), NewCoreValueI32(3)}
	v, err := LiftFlat(core, u8List, nil, mem)
	if err != nil {
		t.Fatalf("LiftFlat: %v", err)
	}
	b, ok := v.([]byte)
	if !ok || string(b) != "abc" {
		t.Fatalf("lifted list<u8> = %#v, want []byte(\"abc\")", v)
	}
	mem[8] = 'z' // mutate the guest memory the lift read from
	if string(b) != "abc" {
		t.Fatal("lifted []byte aliases guest memory instead of copying it")
	}

	// In-memory shape: [ptr:4][len:4] at offset 0 pointing at the same bytes.
	mem[8] = 'a'
	mem[0], mem[4] = 8, 3
	lv, err := Load(mem, 0, u8List, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if lb, ok := lv.([]byte); !ok || string(lb) != "abc" {
		t.Fatalf("loaded list<u8> = %#v, want []byte(\"abc\")", lv)
	}

	// A non-u8 element type stays the general []Value shape.
	u16List := binary.ListDesc{Element: binary.TypeRef{Primitive: "u16"}}
	mem[8], mem[9] = 0x01, 0x00
	v16, err := LiftFlat([]CoreValue{NewCoreValueI32(8), NewCoreValueI32(1)}, u16List, nil, mem)
	if err != nil {
		t.Fatalf("LiftFlat u16: %v", err)
	}
	if _, ok := v16.([]Value); !ok {
		t.Fatalf("lifted list<u16> = %T, want []Value", v16)
	}
}

// TestLiftListU8CompactBounds proves the fast path keeps the general path's
// bounds check, including the ptr+length wraparound case.
func TestLiftListU8CompactBounds(t *testing.T) {
	mem := make([]byte, 16)
	u8List := binary.ListDesc{Element: binary.TypeRef{Primitive: "u8"}}
	if _, err := LiftFlat([]CoreValue{NewCoreValueI32(8), NewCoreValueI32(9)}, u8List, nil, mem); err == nil {
		t.Fatal("expected an out-of-bounds error for len past the end of memory")
	}
	if _, err := LiftFlat([]CoreValue{NewCoreValueI32(0xFFFFFFFF), NewCoreValueI32(2)}, u8List, nil, mem); err == nil {
		t.Fatal("expected an out-of-bounds error for a wrapping ptr+len")
	}
}
