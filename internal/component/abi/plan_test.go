package abi

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// TestLowerStepMatchesLowerFlatInto is the equivalence oracle for the compiled
// lower plan: for every type/value in the flat oracle battery (all primitives,
// string, list, record, variant, enum, flags, option, result, tuple, handles,
// and the spilling cases), CompileLower + LowerStep.Lower must produce
// byte-for-byte the same CoreValues AND write the same linear-memory bytes as
// the tree-walking LowerFlat -- driven by the identical deterministic bump
// allocator, so the embedded pointers match too. Since LowerFlat is itself
// oracle-verified against definitions.py, this transitively holds the compiled
// path to the spec.
func TestLowerStepMatchesLowerFlatInto(t *testing.T) {
	goldenEntries := loadFlatOracleGolden(t)
	battery := loadFlatOracleBattery(t)

	for _, entry := range goldenEntries {
		t.Run(entry.Name, func(t *testing.T) {
			desc, ok := battery.descByName[entry.Type]
			if !ok {
				t.Fatalf("no type %q in battery", entry.Type)
			}
			rawValue, ok := battery.valueMap[fmt.Sprintf("%s:%s", entry.Type, entry.Name)]
			if !ok {
				t.Fatalf("no value for %s:%s", entry.Type, entry.Name)
			}
			v, err := convertTestValue(rawValue, desc, battery.resolve)
			if err != nil {
				t.Fatalf("convert: %v", err)
			}

			// Reference path: tree-walking LowerFlat.
			refMem := make([]byte, 65536)
			refAlloc := newBumpAllocator(entry.MemBase)
			refCore, refErr := LowerFlat(v, desc, battery.resolve, refAlloc.realloc, refMem)

			// Compiled path: CompileLower once, then LowerStep.Lower.
			step, compErr := CompileLower(desc, battery.resolve)
			if compErr != nil {
				t.Fatalf("CompileLower: %v", compErr)
			}
			planMem := make([]byte, 65536)
			planAlloc := newBumpAllocator(entry.MemBase)
			planCore, planErr := step.Lower(nil, v, planAlloc.realloc, planMem)

			// Errors must agree (both nil here -- these are all happy-path values).
			if (refErr == nil) != (planErr == nil) {
				t.Fatalf("error mismatch: LowerFlat err=%v, plan err=%v", refErr, planErr)
			}
			if refErr != nil {
				return
			}

			if len(planCore) != len(refCore) {
				t.Fatalf("core value count: plan %d, ref %d", len(planCore), len(refCore))
			}
			for i := range refCore {
				if planCore[i].Kind != refCore[i].Kind || planCore[i].Bits != refCore[i].Bits {
					t.Errorf("core[%d]: plan {%s 0x%x}, ref {%s 0x%x}",
						i, planCore[i].Kind, planCore[i].Bits, refCore[i].Kind, refCore[i].Bits)
				}
			}
			if !bytes.Equal(planMem, refMem) {
				t.Errorf("linear memory written by the plan differs from LowerFlat")
			}
		})
	}
}

// TestLowerStepErrorParity checks the leaf-kind error branches produce the same
// message shape as the tree-walk (a wrong Go type for a primitive/string/handle
// param).
func TestLowerStepErrorParity(t *testing.T) {
	check := func(name, prim string, badVal Value, wantSubstr string) {
		step, err := CompileLower(binary.PrimitiveDesc{Prim: prim}, nil)
		if err != nil {
			t.Fatalf("%s: CompileLower: %v", name, err)
		}
		_, err = step.Lower(nil, badVal, func(_, _, _, _ uint32) (uint32, error) { return 1024, nil }, make([]byte, 4096))
		if err == nil {
			t.Fatalf("%s: expected an error for a wrong-typed value", name)
		}
		if !bytes.Contains([]byte(err.Error()), []byte(wantSubstr)) {
			t.Fatalf("%s: error %q does not contain %q", name, err.Error(), wantSubstr)
		}
	}

	check("primitive u32", "u32", "not a u32", "expected uint32")
	check("string", "string", uint32(1), "expected string")

	// Unknown primitive: CompileLower classifies it as a primitive step, and
	// Lower surfaces the same "unknown primitive" error the tree-walk does.
	badStep, err := CompileLower(binary.PrimitiveDesc{Prim: "u128"}, nil)
	if err != nil {
		t.Fatalf("CompileLower bogus prim: %v", err)
	}
	if _, err := badStep.Lower(nil, uint32(0), nil, nil); err == nil {
		t.Fatal("expected an error lowering an unknown primitive")
	}
}

// TestLowerStepSpill covers the composite spill branch: a tuple of 17 u32s
// flattens to 17 core values (> MaxFlatParams=16), so CompileLower marks it
// spilling and Lower stores it to memory + returns a single i32 pointer --
// byte-identical to LowerFlat's own spill path.
func TestLowerStepSpill(t *testing.T) {
	u32 := binary.TypeRef{Primitive: "u32"}
	elems := make([]binary.TypeRef, 17)
	for i := range elems {
		elems[i] = u32
	}
	desc := binary.TupleDesc{Elements: elems}
	resolve := func(uint32) binary.TypeDesc { return nil }

	val := make([]Value, 17)
	for i := range val {
		val[i] = uint32(i * 7)
	}

	refMem := make([]byte, 65536)
	refCore, refErr := LowerFlat(val, desc, resolve, newBumpAllocator(1024).realloc, refMem)
	if refErr != nil {
		t.Fatalf("LowerFlat spill: %v", refErr)
	}

	step, err := CompileLower(desc, resolve)
	if err != nil {
		t.Fatalf("CompileLower: %v", err)
	}
	if step.kind != lowerKindComposite || !step.spills {
		t.Fatalf("expected a spilling composite step, got kind=%d spills=%v", step.kind, step.spills)
	}
	planMem := make([]byte, 65536)
	planCore, err := step.Lower(nil, val, newBumpAllocator(1024).realloc, planMem)
	if err != nil {
		t.Fatalf("Lower spill: %v", err)
	}

	if len(planCore) != 1 || len(refCore) != 1 {
		t.Fatalf("spill should flatten to 1 pointer: plan %d, ref %d", len(planCore), len(refCore))
	}
	if planCore[0] != refCore[0] {
		t.Fatalf("spill pointer: plan %v, ref %v", planCore[0], refCore[0])
	}
	if !bytes.Equal(planMem, refMem) {
		t.Fatal("spilled memory differs from LowerFlat")
	}

	// Handle: own/borrow expects a uint32; a wrong type errors.
	hstep, err := CompileLower(binary.OwnDesc{ResourceType: 0}, nil)
	if err != nil {
		t.Fatalf("CompileLower own: %v", err)
	}
	if _, err := hstep.Lower(nil, "notahandle", nil, nil); err == nil || !bytes.Contains([]byte(err.Error()), []byte("handle expected uint32")) {
		t.Fatalf("handle error parity: got %v", err)
	}
}
