package abi

import (
	"bytes"
	"fmt"
	"testing"

	bintype "github.com/samyfodil/wazy/internal/component/binary"
)

// Coverage tests for all primitive types load/store and other edge cases

func TestLoadAllPrimitives(t *testing.T) {
	tests := []struct {
		name  string
		setup func() ([]byte, uint32, bintype.TypeDesc)
		check func(Value) bool
	}{
		{
			name: "load u8",
			setup: func() ([]byte, uint32, bintype.TypeDesc) {
				mem := make([]byte, 10)
				mem[0] = 42
				return mem, 0, bintype.PrimitiveDesc{Prim: "u8"}
			},
			check: func(v Value) bool { return v.(uint32) == 42 },
		},
		{
			name: "load u16",
			setup: func() ([]byte, uint32, bintype.TypeDesc) {
				mem := make([]byte, 10)
				mem[0] = 0x34
				mem[1] = 0x12
				return mem, 0, bintype.PrimitiveDesc{Prim: "u16"}
			},
			check: func(v Value) bool { return v.(uint32) == 0x1234 },
		},
		{
			name: "load u64",
			setup: func() ([]byte, uint32, bintype.TypeDesc) {
				mem := make([]byte, 10)
				mem[0] = 0x78
				mem[1] = 0x56
				mem[2] = 0x34
				mem[3] = 0x12
				mem[4] = 0xef
				mem[5] = 0xcd
				mem[6] = 0xab
				mem[7] = 0x90
				return mem, 0, bintype.PrimitiveDesc{Prim: "u64"}
			},
			check: func(v Value) bool { return v.(uint64) == 0x90abcdef12345678 },
		},
		{
			name: "load s8",
			setup: func() ([]byte, uint32, bintype.TypeDesc) {
				mem := make([]byte, 10)
				mem[0] = 0xFF // -1
				return mem, 0, bintype.PrimitiveDesc{Prim: "s8"}
			},
			check: func(v Value) bool { return v.(int32) == -1 },
		},
		{
			name: "load s16",
			setup: func() ([]byte, uint32, bintype.TypeDesc) {
				mem := make([]byte, 10)
				mem[0] = 0xFF
				mem[1] = 0xFF
				return mem, 0, bintype.PrimitiveDesc{Prim: "s16"}
			},
			check: func(v Value) bool { return v.(int32) == -1 },
		},
		{
			name: "load s64",
			setup: func() ([]byte, uint32, bintype.TypeDesc) {
				mem := make([]byte, 10)
				for i := 0; i < 8; i++ {
					mem[i] = 0xFF
				}
				return mem, 0, bintype.PrimitiveDesc{Prim: "s64"}
			},
			check: func(v Value) bool { return v.(int64) == -1 },
		},
		{
			name: "load f32",
			setup: func() ([]byte, uint32, bintype.TypeDesc) {
				mem := make([]byte, 10)
				// 1.0 in IEEE 754 float32
				mem[0] = 0x00
				mem[1] = 0x00
				mem[2] = 0x80
				mem[3] = 0x3f
				return mem, 0, bintype.PrimitiveDesc{Prim: "f32"}
			},
			check: func(v Value) bool { return v.(float32) == 1.0 },
		},
		{
			name: "load f64",
			setup: func() ([]byte, uint32, bintype.TypeDesc) {
				mem := make([]byte, 10)
				// 1.0 in IEEE 754 float64
				mem[0] = 0x00
				mem[1] = 0x00
				mem[2] = 0x00
				mem[3] = 0x00
				mem[4] = 0x00
				mem[5] = 0x00
				mem[6] = 0xf0
				mem[7] = 0x3f
				return mem, 0, bintype.PrimitiveDesc{Prim: "f64"}
			},
			check: func(v Value) bool { return v.(float64) == 1.0 },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mem, ptr, typeDesc := tt.setup()
			v, err := Load(mem, ptr, typeDesc, nil)
			if err != nil {
				t.Errorf("Load failed: %v", err)
				return
			}
			if !tt.check(v) {
				t.Errorf("value check failed for %v", v)
			}
		})
	}
}

func TestStoreAllPrimitives(t *testing.T) {
	tests := []struct {
		name  string
		prim  string
		value Value
		check func([]byte) bool
	}{
		{
			name:  "store u8",
			prim:  "u8",
			value: uint32(0x42),
			check: func(mem []byte) bool { return mem[0] == 0x42 },
		},
		{
			name:  "store u16",
			prim:  "u16",
			value: uint32(0x1234),
			check: func(mem []byte) bool { return mem[0] == 0x34 && mem[1] == 0x12 },
		},
		{
			name:  "store u32",
			prim:  "u32",
			value: uint32(0x12345678),
			check: func(mem []byte) bool { return mem[0] == 0x78 && mem[3] == 0x12 },
		},
		{
			name:  "store s8",
			prim:  "s8",
			value: int32(-1),
			check: func(mem []byte) bool { return mem[0] == 0xFF },
		},
		{
			name:  "store s16",
			prim:  "s16",
			value: int32(-1),
			check: func(mem []byte) bool { return mem[0] == 0xFF && mem[1] == 0xFF },
		},
		{
			name:  "store f32",
			prim:  "f32",
			value: float32(1.0),
			check: func(mem []byte) bool { return mem[0] == 0x00 && mem[3] == 0x3f },
		},
		{
			name:  "store f64",
			prim:  "f64",
			value: float64(1.0),
			check: func(mem []byte) bool { return mem[7] == 0x3f },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mem := make([]byte, 100)
			err := storePrimitive(mem, 0, tt.prim, tt.value, ReallocFunc(func(_, _, _, _ uint32) (uint32, error) { return 0, nil }))
			if err != nil {
				t.Errorf("Store failed: %v", err)
				return
			}
			if !tt.check(mem) {
				t.Errorf("memory check failed")
			}
		})
	}
}

func TestRecordWithMultipleFields(t *testing.T) {
	mem := make([]byte, 100)

	desc := bintype.RecordDesc{
		Fields: []bintype.RecordField{
			{Name: "a", Type: bintype.TypeRef{Primitive: "u8"}},
			{Name: "b", Type: bintype.TypeRef{Primitive: "u32"}},
			{Name: "c", Type: bintype.TypeRef{Primitive: "u16"}},
		},
	}

	resolve := func(idx uint32) bintype.TypeDesc { return nil }
	value := []Value{uint32(1), uint32(0x12345678), uint32(0x1234)}

	err := storeRecord(mem, 0, value, desc, resolve, ReallocFunc(func(_, _, _, _ uint32) (uint32, error) { return 0, nil }))
	if err != nil {
		t.Errorf("storeRecord failed: %v", err)
	}

	// Load it back
	loaded, err := loadRecord(mem, 0, desc, resolve)
	if err != nil {
		t.Errorf("loadRecord failed: %v", err)
	}

	if len(loaded.([]Value)) != 3 {
		t.Errorf("expected 3 fields, got %d", len(loaded.([]Value)))
	}
}

func TestTupleWithMultipleElements(t *testing.T) {
	mem := make([]byte, 100)

	desc := bintype.TupleDesc{
		Elements: []bintype.TypeRef{
			{Primitive: "u8"},
			{Primitive: "u32"},
			{Primitive: "u16"},
		},
	}

	resolve := func(idx uint32) bintype.TypeDesc { return nil }
	value := []Value{uint32(1), uint32(0x12345678), uint32(0x1234)}

	err := storeTuple(mem, 0, value, desc, resolve, ReallocFunc(func(_, _, _, _ uint32) (uint32, error) { return 0, nil }))
	if err != nil {
		t.Errorf("storeTuple failed: %v", err)
	}

	// Load it back
	loaded, err := loadTuple(mem, 0, desc, resolve)
	if err != nil {
		t.Errorf("loadTuple failed: %v", err)
	}

	if len(loaded.([]Value)) != 3 {
		t.Errorf("expected 3 elements, got %d", len(loaded.([]Value)))
	}
}

func TestVariantWithPayload(t *testing.T) {
	mem := make([]byte, 100)

	desc := bintype.VariantDesc{
		Cases: []bintype.VariantCase{
			{Name: "a", Type: &bintype.TypeRef{Primitive: "u32"}},
			{Name: "b", Type: nil},
			{Name: "c", Type: &bintype.TypeRef{Primitive: "u16"}},
		},
	}

	resolve := func(idx uint32) bintype.TypeDesc { return nil }

	// Test case 0 with payload
	value := VariantValue{Disc: 0, Payload: uint32(42)}
	err := storeVariant(mem, 0, value, desc, 0, resolve, ReallocFunc(func(_, _, _, _ uint32) (uint32, error) { return 0, nil }))
	if err != nil {
		t.Errorf("storeVariant case 0 failed: %v", err)
	}

	// Load it back
	loaded, err := loadVariant(mem, 0, desc, resolve)
	if err != nil {
		t.Errorf("loadVariant failed: %v", err)
	}

	lv := loaded.(VariantValue)
	if lv.Disc != 0 {
		t.Errorf("expected disc 0, got %d", lv.Disc)
	}
}

func TestEmptyString(t *testing.T) {
	mem := make([]byte, 100)

	err := storeString(mem, 0, "", ReallocFunc(func(_, _, _, _ uint32) (uint32, error) {
		return 100, nil
	}))
	if err != nil {
		t.Errorf("storeString empty failed: %v", err)
	}

	// Load it back
	loaded, err := loadString(mem, 0)
	if err != nil {
		t.Errorf("loadString empty failed: %v", err)
	}

	if loaded != "" {
		t.Errorf("expected empty string, got %q", loaded)
	}
}

func TestLoadIntVariousSizes(t *testing.T) {
	mem := make([]byte, 100)

	// Test different sizes
	tests := []struct {
		nbytes uint32
		setup  func() uint32
	}{
		{1, func() uint32 {
			mem[0] = 0x42
			return 1
		}},
		{2, func() uint32 {
			mem[0] = 0x34
			mem[1] = 0x12
			return 2
		}},
		{4, func() uint32 {
			mem[0] = 0x78
			mem[1] = 0x56
			mem[2] = 0x34
			mem[3] = 0x12
			return 4
		}},
		{8, func() uint32 {
			for i := 0; i < 8; i++ {
				mem[i] = byte(i + 1)
			}
			return 8
		}},
	}

	for _, tt := range tests {
		tt.setup()
		v, err := loadInt(mem, 0, tt.nbytes, false)
		if err != nil {
			t.Errorf("loadInt nbytes=%d failed: %v", tt.nbytes, err)
		}
		if v == nil {
			t.Errorf("loadInt nbytes=%d returned nil", tt.nbytes)
		}
	}
}

// TestStoreValueAlignArgMatchesDerived proves storeValue's two paths agree:
// the fast one, where the caller hands in the type's own alignment (every
// caller already computed it to place ptr), and the slow one, where the
// option/variant/result case derives it by walking the type. They must write
// byte-identical memory, including for a variant whose discriminant is WIDER
// than any case payload -- the one shape where "the variant's alignment" and
// "MaxCaseAlignment" are different numbers that must still land on the same
// payload offset.
func TestStoreValueAlignArgMatchesDerived(t *testing.T) {
	u8 := bintype.TypeRef{Primitive: "u8"}
	u32 := bintype.TypeRef{Primitive: "u32"}
	u64 := bintype.TypeRef{Primitive: "u64"}
	str := bintype.TypeRef{Primitive: "string"}

	// A variant with 300 cases: its discriminant is u16 (alignment 2) while no
	// case payload needs more than 1.
	wideDisc := bintype.VariantDesc{}
	for i := 0; i < 300; i++ {
		c := bintype.VariantCase{Name: fmt.Sprintf("c%d", i)}
		if i == 299 {
			c.Type = &u8
		}
		wideDisc.Cases = append(wideDisc.Cases, c)
	}

	optU64 := bintype.OptionDesc{Element: u64}
	idx := func(i uint32) *uint32 { return &i }
	resolve := mapResolver(map[uint32]bintype.TypeDesc{
		0: optU64,
		1: bintype.ResultDesc{Ok: &u32, Err: &u8},
	})
	optU64Ref := bintype.TypeRef{TypeIndex: idx(0)}
	resU32U8Ref := bintype.TypeRef{TypeIndex: idx(1)}

	tests := []struct {
		name string
		desc bintype.TypeDesc
		val  Value
	}{
		{"option<u64> some", optU64, uint64(0x0102030405060708)},
		{"option<u64> none", optU64, nil},
		{"option<u8>", bintype.OptionDesc{Element: u8}, uint32(3)},
		{"option<string>", bintype.OptionDesc{Element: str}, "hi"},
		{"result<u64,u8> ok", bintype.ResultDesc{Ok: &u64, Err: &u8}, ResultValue{Payload: uint64(9)}},
		{"result<u8,u64> err", bintype.ResultDesc{Ok: &u8, Err: &u64}, ResultValue{IsErr: true, Payload: uint64(9)}},
		{"result<_,u8> err", bintype.ResultDesc{Err: &u8}, ResultValue{IsErr: true, Payload: uint32(5)}},
		{"result<u32,u8> ok", bintype.ResultDesc{Ok: &u32, Err: &u8}, ResultValue{Payload: uint32(0x11223344)}},
		{"result<option<u64>,u8> ok", bintype.ResultDesc{Ok: &optU64Ref, Err: &u8}, ResultValue{Payload: uint64(9)}},
		{"variant u8 disc", bintype.VariantDesc{Cases: []bintype.VariantCase{{Name: "a", Type: &u8}, {Name: "b", Type: &u64}}}, VariantValue{Disc: 1, Payload: uint64(9)}},
		{"variant u16 disc wider than cases", wideDisc, VariantValue{Disc: 299, Payload: uint32(7)}},
		{"record{u8, option<u64>}", bintype.RecordDesc{Fields: []bintype.RecordField{{Name: "a", Type: u8}, {Name: "b", Type: optU64Ref}}}, []Value{uint32(1), uint64(2)}},
		{"tuple<u8, result<u32,u8>>", bintype.TupleDesc{Elements: []bintype.TypeRef{u8, resU32U8Ref}}, []Value{uint32(1), ResultValue{Payload: uint32(2)}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			align, err := Alignment(tt.desc, resolve)
			if err != nil {
				t.Fatalf("Alignment: %v", err)
			}
			for _, ptr := range []uint32{0, align * 3} {
				fast := make([]byte, 8192)
				slow := make([]byte, 8192)
				if err := storeValue(fast, ptr, tt.desc, tt.val, align, resolve, okRealloc); err != nil {
					t.Fatalf("storeValue(align=%d) at ptr %d: %v", align, ptr, err)
				}
				if err := storeValue(slow, ptr, tt.desc, tt.val, 0, resolve, okRealloc); err != nil {
					t.Fatalf("storeValue(align=0) at ptr %d: %v", ptr, err)
				}
				if !bytes.Equal(fast, slow) {
					t.Fatalf("ptr %d: align=%d and align=0 wrote different memory", ptr, align)
				}
			}
		})
	}
}
