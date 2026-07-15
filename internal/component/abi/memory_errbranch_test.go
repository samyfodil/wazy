package abi

import (
	"strings"
	"testing"

	bintype "github.com/samyfodil/wazy/internal/component/binary"
)

// u32ptr returns a pointer to a uint32 literal, for TypeRef.TypeIndex.
func u32ptr(v uint32) *uint32 { return &v }

// okRealloc is a realloc that always succeeds at a fixed address.
func okRealloc(_, _, _, _ uint32) (uint32, error) { return 4096, nil }

// failRealloc always returns an error.
func failRealloc(_, _, _, _ uint32) (uint32, error) {
	return 0, errFake
}

var errFake = &fakeErr{}

type fakeErr struct{}

func (*fakeErr) Error() string { return "fake realloc failure" }

// funcResolve returns an unsupported type (FuncDesc) for any index, so that
// Size/Alignment fail — used to exercise nested layout-error branches.
func funcResolve(_ uint32) bintype.TypeDesc { return bintype.FuncDesc{} }

// ---------- storeInt direct edge cases ----------

func TestStoreIntUnsupportedType(t *testing.T) {
	mem := make([]byte, 8)
	if err := storeInt(mem, 0, "not-an-int", 4); err == nil {
		t.Error("expected error for unsupported storeInt value type")
	}
}

func TestStoreIntInvalidNBytes(t *testing.T) {
	mem := make([]byte, 8)
	if err := storeInt(mem, 0, uint32(1), 3); err == nil {
		t.Error("expected error for invalid nbytes=3")
	}
}

func TestStoreIntBufferOverflow(t *testing.T) {
	mem := make([]byte, 2)
	if err := storeInt(mem, 0, uint32(1), 4); err == nil {
		t.Error("expected error for storeInt buffer overflow")
	}
}

func TestStoreIntAllWidths(t *testing.T) {
	mem := make([]byte, 8)
	for _, w := range []uint32{1, 2, 4, 8} {
		if err := storeInt(mem, 0, uint64(1), w); err != nil {
			t.Errorf("storeInt width %d: %v", w, err)
		}
	}
}

// ---------- loadInt direct edge cases ----------

func TestLoadIntBufferOverflow(t *testing.T) {
	mem := make([]byte, 4)
	if _, err := loadInt(mem, 0, 8, false); err == nil {
		t.Error("expected error for loadInt buffer overflow at 8 bytes")
	}
}

func TestLoadIntInvalidNBytes(t *testing.T) {
	mem := make([]byte, 8)
	if _, err := loadInt(mem, 0, 3, false); err == nil {
		t.Error("expected error for loadInt invalid nbytes=3")
	}
}

func TestLoadIntSignedWidths(t *testing.T) {
	mem := []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
	// signed 2 bytes
	if v, _ := loadInt(mem, 0, 2, true); v.(int32) != -1 {
		t.Errorf("signed 2-byte load = %v, want -1", v)
	}
	// signed 8 bytes
	if v, _ := loadInt(mem, 0, 8, true); v.(int64) != -1 {
		t.Errorf("signed 8-byte load = %v, want -1", v)
	}
	// unsigned 8 bytes
	if v, _ := loadInt(mem, 0, 8, false); v.(uint64) == 0 {
		t.Error("unsigned 8-byte load unexpectedly 0")
	}
}

// ---------- storeString error branches ----------

func TestStoreStringReallocFails(t *testing.T) {
	mem := make([]byte, 100)
	if err := storeString(mem, 0, "hi", failRealloc); err == nil {
		t.Error("expected error for storeString realloc failure")
	}
}

func TestStoreStringAllocOutOfBounds(t *testing.T) {
	mem := make([]byte, 10)
	// realloc returns a pointer past the end of mem
	badRealloc := func(_, _, _, _ uint32) (uint32, error) { return 1000, nil }
	if err := storeString(mem, 0, "hello", badRealloc); err == nil {
		t.Error("expected error for storeString alloc out of bounds")
	}
}

func TestStoreStringPtrStoreOverflow(t *testing.T) {
	// mem large enough for string data (at 4096) but the ptr/len header at ptr=... won't fit.
	mem := make([]byte, 5000)
	// ptr near the end: writing 8-byte header overflows.
	if err := storeString(mem, 4998, "x", okRealloc); err == nil {
		t.Error("expected error for storeString header store overflow")
	}
}

// ---------- loadString error branches ----------

func TestLoadStringHeaderOverflow(t *testing.T) {
	mem := make([]byte, 4)
	if _, err := loadString(mem, 0); err == nil {
		t.Error("expected error for loadString header overflow")
	}
}

func TestLoadStringDataOutOfBounds(t *testing.T) {
	mem := make([]byte, 20)
	// header at 0: ptr=100 (out of range), len=5
	mustStoreInt(t, mem, 0, uint32(100), 4)
	mustStoreInt(t, mem, 4, uint32(5), 4)
	if _, err := loadString(mem, 0); err == nil {
		t.Error("expected error for loadString data out of bounds")
	}
}

// ---------- storeList error branches ----------

func TestStoreListSizeError(t *testing.T) {
	mem := make([]byte, 100)
	// non-empty list with an unsupported element type -> Size() errors
	err := storeList(mem, 0, []Value{uint32(1)}, bintype.FuncDesc{}, nil, okRealloc)
	if err == nil {
		t.Error("expected error for storeList element Size failure")
	}
}

func TestStoreListReallocFails(t *testing.T) {
	mem := make([]byte, 100)
	err := storeList(mem, 0, []Value{uint32(1)}, bintype.PrimitiveDesc{Prim: "u32"}, nil, failRealloc)
	if err == nil {
		t.Error("expected error for storeList realloc failure")
	}
}

func TestStoreListAllocOutOfBounds(t *testing.T) {
	mem := make([]byte, 20)
	badRealloc := func(_, _, _, _ uint32) (uint32, error) { return 1000, nil }
	err := storeList(mem, 0, []Value{uint32(1)}, bintype.PrimitiveDesc{Prim: "u32"}, nil, badRealloc)
	if err == nil {
		t.Error("expected error for storeList alloc out of bounds")
	}
}

func TestStoreListElementError(t *testing.T) {
	mem := make([]byte, 5000)
	// element type u32 but a value is the wrong Go type -> storeValue fails
	err := storeList(mem, 0, []Value{"not-a-u32"}, bintype.PrimitiveDesc{Prim: "u32"}, nil, okRealloc)
	if err == nil {
		t.Error("expected error for storeList element store failure")
	}
}

func TestStoreListHeaderOverflow(t *testing.T) {
	// list data allocated at 4096; header write at ptr near end overflows.
	mem := make([]byte, 5000)
	err := storeList(mem, 4998, []Value{uint32(1)}, bintype.PrimitiveDesc{Prim: "u32"}, nil, okRealloc)
	if err == nil {
		t.Error("expected error for storeList header overflow")
	}
}

// ---------- loadList error branches ----------

func TestLoadListSizeError(t *testing.T) {
	mem := make([]byte, 100)
	// header ptr=0,len=1 with unsupported element -> Size errors
	mustStoreInt(t, mem, 0, uint32(0), 4)
	mustStoreInt(t, mem, 4, uint32(1), 4)
	if _, err := loadList(mem, 0, bintype.FuncDesc{}, nil); err == nil {
		t.Error("expected error for loadList element Size failure")
	}
}

func TestLoadListElementError(t *testing.T) {
	// A list of one string: the outer list range is valid, but the inner
	// string's data pointer is out of bounds, so the per-element load fails.
	mem := make([]byte, 16)
	mustStoreInt(t, mem, 0, uint32(8), 4) // list data ptr
	mustStoreInt(t, mem, 4, uint32(1), 4) // list length 1
	// string header at offset 8: data ptr=100 (out of range), len=5
	mustStoreInt(t, mem, 8, uint32(100), 4)
	mustStoreInt(t, mem, 12, uint32(5), 4)
	if _, err := loadList(mem, 0, bintype.PrimitiveDesc{Prim: "string"}, nil); err == nil {
		t.Error("expected error for loadList element (string) load failure")
	}
}

// ---------- storeValue / loadValue dispatch error branches ----------

func TestStoreValueUnsupportedType(t *testing.T) {
	mem := make([]byte, 8)
	if err := storeValue(mem, 0, bintype.FuncDesc{}, uint32(1), nil, okRealloc); err == nil {
		t.Error("expected error for storeValue unsupported type")
	}
}

func TestLoadValueUnsupportedType(t *testing.T) {
	mem := make([]byte, 8)
	if _, err := loadValue(mem, 0, bintype.FuncDesc{}, nil); err == nil {
		t.Error("expected error for loadValue unsupported type")
	}
}

func TestStoreValueListBadElementRef(t *testing.T) {
	mem := make([]byte, 8)
	// ListDesc with an empty TypeRef (neither prim nor index) -> resolveType error
	desc := bintype.ListDesc{Element: bintype.TypeRef{}}
	if err := storeValue(mem, 0, desc, []Value{}, nil, okRealloc); err == nil {
		t.Error("expected error for storeValue list bad element ref")
	}
}

func TestLoadValueListBadElementRef(t *testing.T) {
	mem := make([]byte, 8)
	desc := bintype.ListDesc{Element: bintype.TypeRef{}}
	if _, err := loadValue(mem, 0, desc, nil); err == nil {
		t.Error("expected error for loadValue list bad element ref")
	}
}

func TestStoreValueOptionBadElementRef(t *testing.T) {
	mem := make([]byte, 8)
	desc := bintype.OptionDesc{Element: bintype.TypeRef{}}
	if err := storeValue(mem, 0, desc, nil, nil, okRealloc); err == nil {
		t.Error("expected error for storeValue option bad element ref")
	}
}

func TestLoadValueOptionBadElementRef(t *testing.T) {
	mem := make([]byte, 8)
	desc := bintype.OptionDesc{Element: bintype.TypeRef{}}
	if _, err := loadValue(mem, 0, desc, nil); err == nil {
		t.Error("expected error for loadValue option bad element ref")
	}
}

// ---------- storeValue handle type mismatch ----------

func TestStoreHandleWrongType(t *testing.T) {
	mem := make([]byte, 8)
	if err := storeValue(mem, 0, bintype.OwnDesc{}, "not-a-handle", nil, okRealloc); err == nil {
		t.Error("expected error for storeValue own handle wrong type")
	}
	if err := storeValue(mem, 0, bintype.BorrowDesc{}, "not-a-handle", nil, okRealloc); err == nil {
		t.Error("expected error for storeValue borrow handle wrong type")
	}
}

func TestStoreLoadHandleRoundTrip(t *testing.T) {
	mem := make([]byte, 8)
	if err := storeValue(mem, 0, bintype.OwnDesc{}, uint32(7), nil, okRealloc); err != nil {
		t.Fatalf("storeValue own handle: %v", err)
	}
	v, err := loadValue(mem, 0, bintype.OwnDesc{}, nil)
	if err != nil {
		t.Fatalf("loadValue own handle: %v", err)
	}
	if v.(uint32) != 7 {
		t.Errorf("handle round trip = %v, want 7", v)
	}
}

// ---------- record nested layout errors ----------

func TestStoreRecordFieldAlignmentError(t *testing.T) {
	mem := make([]byte, 100)
	desc := bintype.RecordDesc{Fields: []bintype.RecordField{
		{Name: "a", Type: bintype.TypeRef{TypeIndex: u32ptr(0)}},
	}}
	err := storeRecord(mem, 0, []Value{uint32(1)}, desc, funcResolve, okRealloc)
	if err == nil {
		t.Error("expected error for storeRecord field alignment failure")
	}
}

func TestStoreRecordFieldStoreError(t *testing.T) {
	mem := make([]byte, 8)
	desc := bintype.RecordDesc{Fields: []bintype.RecordField{
		{Name: "a", Type: bintype.TypeRef{Primitive: "u32"}},
		{Name: "b", Type: bintype.TypeRef{Primitive: "u32"}},
	}}
	// mem only 8 bytes; second field write is within bounds, but value type wrong
	err := storeRecord(mem, 0, []Value{uint32(1), "bad"}, desc, nil, okRealloc)
	if err == nil {
		t.Error("expected error for storeRecord field store failure")
	}
}

func TestLoadRecordFieldLoadError(t *testing.T) {
	mem := make([]byte, 4) // room for only one u32
	desc := bintype.RecordDesc{Fields: []bintype.RecordField{
		{Name: "a", Type: bintype.TypeRef{Primitive: "u32"}},
		{Name: "b", Type: bintype.TypeRef{Primitive: "u32"}},
	}}
	if _, err := loadRecord(mem, 0, desc, nil); err == nil {
		t.Error("expected error for loadRecord field load out of bounds")
	}
}

// ---------- tuple nested layout errors ----------

func TestStoreTupleElementAlignmentError(t *testing.T) {
	mem := make([]byte, 100)
	desc := bintype.TupleDesc{Elements: []bintype.TypeRef{{TypeIndex: u32ptr(0)}}}
	err := storeTuple(mem, 0, []Value{uint32(1)}, desc, funcResolve, okRealloc)
	if err == nil {
		t.Error("expected error for storeTuple element alignment failure")
	}
}

func TestStoreTupleElementStoreError(t *testing.T) {
	mem := make([]byte, 8)
	desc := bintype.TupleDesc{Elements: []bintype.TypeRef{
		{Primitive: "u32"},
		{Primitive: "u32"},
	}}
	err := storeTuple(mem, 0, []Value{uint32(1), "bad"}, desc, nil, okRealloc)
	if err == nil {
		t.Error("expected error for storeTuple element store failure")
	}
}

func TestLoadTupleElementLoadError(t *testing.T) {
	mem := make([]byte, 4)
	desc := bintype.TupleDesc{Elements: []bintype.TypeRef{
		{Primitive: "u32"},
		{Primitive: "u32"},
	}}
	if _, err := loadTuple(mem, 0, desc, nil); err == nil {
		t.Error("expected error for loadTuple element load out of bounds")
	}
}

// ---------- variant nested layout errors ----------

func TestStoreVariantMaxCaseAlignError(t *testing.T) {
	mem := make([]byte, 100)
	desc := bintype.VariantDesc{Cases: []bintype.VariantCase{
		{Name: "a", Type: &bintype.TypeRef{TypeIndex: u32ptr(0)}},
	}}
	err := storeVariant(mem, 0, VariantValue{Disc: 0, Payload: uint32(1)}, desc, funcResolve, okRealloc)
	if err == nil {
		t.Error("expected error for storeVariant max-case-alignment failure")
	}
}

func TestStoreVariantPayloadStoreError(t *testing.T) {
	mem := make([]byte, 100)
	desc := bintype.VariantDesc{Cases: []bintype.VariantCase{
		{Name: "a", Type: &bintype.TypeRef{Primitive: "u32"}},
	}}
	// payload wrong Go type
	err := storeVariant(mem, 0, VariantValue{Disc: 0, Payload: "bad"}, desc, nil, okRealloc)
	if err == nil {
		t.Error("expected error for storeVariant payload store failure")
	}
}

func TestLoadVariantPayloadLoadError(t *testing.T) {
	// discriminant fits, but payload u32 read overflows
	mem := make([]byte, 2)
	desc := bintype.VariantDesc{Cases: []bintype.VariantCase{
		{Name: "a", Type: &bintype.TypeRef{Primitive: "u32"}},
	}}
	if _, err := loadVariant(mem, 0, desc, nil); err == nil {
		t.Error("expected error for loadVariant payload load out of bounds")
	}
}

// ---------- enum size error ----------

func TestLoadEnumInvalidLabels(t *testing.T) {
	mem := make([]byte, 8)
	desc := bintype.EnumDesc{Cases: make([]string, 33)} // >32 labels -> size error
	if _, err := loadEnum(mem, 0, desc); err == nil {
		t.Error("expected error for loadEnum invalid label count")
	}
}

func TestStoreEnumInvalidLabels(t *testing.T) {
	mem := make([]byte, 8)
	desc := bintype.EnumDesc{Cases: make([]string, 33)}
	// index 0 is < len, so range check passes; size computation fails
	if err := storeEnum(mem, 0, uint32(0), desc); err == nil {
		t.Error("expected error for storeEnum invalid label count")
	}
}

// ---------- option nested layout errors ----------

func TestStoreOptionAlignmentError(t *testing.T) {
	mem := make([]byte, 100)
	// some payload; element type unsupported -> Alignment error
	if err := storeOption(mem, 0, uint32(1), bintype.FuncDesc{}, funcResolve, okRealloc); err == nil {
		t.Error("expected error for storeOption alignment failure")
	}
}

func TestStoreOptionPayloadError(t *testing.T) {
	mem := make([]byte, 100)
	// element u32, but payload wrong type
	if err := storeOption(mem, 0, "bad", bintype.PrimitiveDesc{Prim: "u32"}, nil, okRealloc); err == nil {
		t.Error("expected error for storeOption payload store failure")
	}
}

func TestLoadOptionInvalidDiscriminant(t *testing.T) {
	mem := make([]byte, 8)
	mem[0] = 5 // invalid option discriminant
	if _, err := loadOption(mem, 0, bintype.PrimitiveDesc{Prim: "u32"}, nil); err == nil {
		t.Error("expected error for loadOption invalid discriminant")
	}
}

func TestLoadOptionPayloadError(t *testing.T) {
	mem := make([]byte, 2)
	mem[0] = 1 // some
	if _, err := loadOption(mem, 0, bintype.PrimitiveDesc{Prim: "u32"}, nil); err == nil {
		t.Error("expected error for loadOption payload load out of bounds")
	}
}

// ---------- result nested layout errors ----------

func TestStoreResultOkAlignmentError(t *testing.T) {
	mem := make([]byte, 100)
	desc := bintype.ResultDesc{Ok: &bintype.TypeRef{TypeIndex: u32ptr(0)}}
	err := storeResult(mem, 0, ResultValue{IsErr: false, Payload: uint32(1)}, desc, funcResolve, okRealloc)
	if err == nil {
		t.Error("expected error for storeResult ok alignment failure")
	}
}

func TestStoreResultErrAlignmentError(t *testing.T) {
	mem := make([]byte, 100)
	desc := bintype.ResultDesc{Err: &bintype.TypeRef{TypeIndex: u32ptr(0)}}
	err := storeResult(mem, 0, ResultValue{IsErr: true, Payload: uint32(1)}, desc, funcResolve, okRealloc)
	if err == nil {
		t.Error("expected error for storeResult err alignment failure")
	}
}

func TestStoreResultOkPayloadError(t *testing.T) {
	mem := make([]byte, 100)
	desc := bintype.ResultDesc{
		Ok:  &bintype.TypeRef{Primitive: "u32"},
		Err: &bintype.TypeRef{Primitive: "u32"},
	}
	err := storeResult(mem, 0, ResultValue{IsErr: false, Payload: "bad"}, desc, nil, okRealloc)
	if err == nil {
		t.Error("expected error for storeResult ok payload store failure")
	}
}

func TestStoreResultErrPayloadError(t *testing.T) {
	mem := make([]byte, 100)
	desc := bintype.ResultDesc{
		Ok:  &bintype.TypeRef{Primitive: "u32"},
		Err: &bintype.TypeRef{Primitive: "u32"},
	}
	err := storeResult(mem, 0, ResultValue{IsErr: true, Payload: "bad"}, desc, nil, okRealloc)
	if err == nil {
		t.Error("expected error for storeResult err payload store failure")
	}
}

func TestLoadResultOkAlignmentError(t *testing.T) {
	mem := make([]byte, 8)
	mem[0] = 0 // ok
	desc := bintype.ResultDesc{Ok: &bintype.TypeRef{TypeIndex: u32ptr(0)}}
	if _, err := loadResult(mem, 0, desc, funcResolve); err == nil {
		t.Error("expected error for loadResult ok alignment failure")
	}
}

func TestLoadResultErrAlignmentError(t *testing.T) {
	mem := make([]byte, 8)
	mem[0] = 1 // err
	desc := bintype.ResultDesc{Err: &bintype.TypeRef{TypeIndex: u32ptr(0)}}
	if _, err := loadResult(mem, 0, desc, funcResolve); err == nil {
		t.Error("expected error for loadResult err alignment failure")
	}
}

func TestLoadResultInvalidDiscriminant(t *testing.T) {
	mem := make([]byte, 8)
	mem[0] = 9 // invalid
	desc := bintype.ResultDesc{
		Ok:  &bintype.TypeRef{Primitive: "u32"},
		Err: &bintype.TypeRef{Primitive: "u32"},
	}
	if _, err := loadResult(mem, 0, desc, nil); err == nil {
		t.Error("expected error for loadResult invalid discriminant")
	}
}

func TestLoadResultOkPayloadError(t *testing.T) {
	mem := make([]byte, 2) // disc byte + not enough for u32 payload
	mem[0] = 0
	desc := bintype.ResultDesc{Ok: &bintype.TypeRef{Primitive: "u32"}}
	if _, err := loadResult(mem, 0, desc, nil); err == nil {
		t.Error("expected error for loadResult ok payload load out of bounds")
	}
}

func TestLoadResultErrPayloadError(t *testing.T) {
	mem := make([]byte, 2)
	mem[0] = 1
	desc := bintype.ResultDesc{Err: &bintype.TypeRef{Primitive: "u32"}}
	if _, err := loadResult(mem, 0, desc, nil); err == nil {
		t.Error("expected error for loadResult err payload load out of bounds")
	}
}

// ---------- top-level Store/Load layout errors ----------

func TestStoreAlignmentComputeError(t *testing.T) {
	mem := make([]byte, 8)
	if err := Store(mem, 0, bintype.FuncDesc{}, uint32(1), nil, okRealloc); err == nil {
		t.Error("expected error for Store on unsupported type (Alignment)")
	}
}

func TestLoadAlignmentComputeError(t *testing.T) {
	mem := make([]byte, 8)
	if _, err := Load(mem, 0, bintype.FuncDesc{}, nil); err == nil {
		t.Error("expected error for Load on unsupported type (Alignment)")
	}
}

func TestStoreSizeComputeErrorAfterAlign(t *testing.T) {
	// A list has a valid alignment (4) but if the mem is too small, the bounds
	// check in Store triggers using a valid Size. Use a huge string type instead:
	// string has alignment 4, size 8; make mem 4 bytes -> bounds error.
	mem := make([]byte, 4)
	if err := Store(mem, 0, bintype.PrimitiveDesc{Prim: "string"}, "x", nil, okRealloc); err == nil {
		t.Error("expected error for Store bounds via size")
	}
}

// ---------- helpers ----------

func mustStoreInt(t *testing.T, mem []byte, ptr uint32, v any, nbytes uint32) {
	t.Helper()
	if err := storeInt(mem, ptr, v, nbytes); err != nil {
		t.Fatalf("mustStoreInt: %v", err)
	}
}

// sanity: error messages are wrapped (spot-check a couple)
func TestErrorMessagesWrapped(t *testing.T) {
	mem := make([]byte, 100)
	err := storeList(mem, 0, []Value{"bad"}, bintype.PrimitiveDesc{Prim: "u32"}, nil, okRealloc)
	if err == nil || !strings.Contains(err.Error(), "storeList") {
		t.Errorf("expected wrapped storeList error, got %v", err)
	}
}
