package binary

import (
	"bytes"
	"testing"
)

// TestRichComponent_Descriptors verifies the descriptor model against rich_component.wasm.
// Ground truth (via `wasm-tools print`):
//
//	type0 enum[red,green,blue]
//	type1 record{x:s32,y:s32}
//	type2 option<string>
//	type3 result<u32,string>
//	type4 list<u64>
//	type5 variant{circle:f64, empty}
//	type6 flags[a,b,c]
//	type7 instance
//	type8 func
func TestRichComponent_Descriptors(t *testing.T) {
	data, err := fixtureFS.ReadFile("testdata/rich_component.wasm")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	c, err := Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	// Verify we have 9 types.
	if len(c.Types) != 9 {
		t.Fatalf("expected 9 types, got %d", len(c.Types))
	}

	// Type 0: enum[red, green, blue]
	{
		typ := c.Types[0]
		if typ.Index != 0 {
			t.Errorf("type[0] index: got %d, want 0", typ.Index)
		}
		if typ.Kind != "enum" {
			t.Errorf("type[0] kind: got %q, want enum", typ.Kind)
		}
		desc, ok := typ.Descriptor.(EnumDesc)
		if !ok {
			t.Errorf("type[0] descriptor: got %T, want EnumDesc", typ.Descriptor)
		} else if len(desc.Cases) != 3 {
			t.Errorf("type[0] enum cases: got %d, want 3", len(desc.Cases))
		} else {
			expectedCases := []string{"red", "green", "blue"}
			for i, want := range expectedCases {
				if desc.Cases[i] != want {
					t.Errorf("type[0] enum case[%d]: got %q, want %q", i, desc.Cases[i], want)
				}
			}
		}
	}

	// Type 1: record{x:s32, y:s32}
	{
		typ := c.Types[1]
		if typ.Kind != "record" {
			t.Errorf("type[1] kind: got %q, want record", typ.Kind)
		}
		desc, ok := typ.Descriptor.(RecordDesc)
		if !ok {
			t.Errorf("type[1] descriptor: got %T, want RecordDesc", typ.Descriptor)
		} else if len(desc.Fields) != 2 {
			t.Errorf("type[1] record fields: got %d, want 2", len(desc.Fields))
		} else {
			// Check field names and types.
			expectedFields := []struct {
				name      string
				primitive string
			}{
				{"x", "s32"},
				{"y", "s32"},
			}
			for i, want := range expectedFields {
				if desc.Fields[i].Name != want.name {
					t.Errorf("type[1] field[%d] name: got %q, want %q", i, desc.Fields[i].Name, want.name)
				}
				if desc.Fields[i].Type.Primitive != want.primitive {
					t.Errorf("type[1] field[%d] type: got %q, want %q", i, desc.Fields[i].Type.Primitive, want.primitive)
				}
			}
		}
	}

	// Type 2: option<string>
	{
		typ := c.Types[2]
		if typ.Kind != "option" {
			t.Errorf("type[2] kind: got %q, want option", typ.Kind)
		}
		desc, ok := typ.Descriptor.(OptionDesc)
		if !ok {
			t.Errorf("type[2] descriptor: got %T, want OptionDesc", typ.Descriptor)
		} else if desc.Element.Primitive != "string" {
			t.Errorf("type[2] option element: got %q, want string", desc.Element.Primitive)
		}
	}

	// Type 3: result<u32, string>
	{
		typ := c.Types[3]
		if typ.Kind != "result" {
			t.Errorf("type[3] kind: got %q, want result", typ.Kind)
		}
		desc, ok := typ.Descriptor.(ResultDesc)
		if !ok {
			t.Errorf("type[3] descriptor: got %T, want ResultDesc", typ.Descriptor)
		} else {
			if desc.Ok == nil || desc.Ok.Primitive != "u32" {
				t.Errorf("type[3] result ok: got %v, want u32", desc.Ok)
			}
			if desc.Err == nil || desc.Err.Primitive != "string" {
				t.Errorf("type[3] result err: got %v, want string", desc.Err)
			}
		}
	}

	// Type 4: list<u64>
	{
		typ := c.Types[4]
		if typ.Kind != "list" {
			t.Errorf("type[4] kind: got %q, want list", typ.Kind)
		}
		desc, ok := typ.Descriptor.(ListDesc)
		if !ok {
			t.Errorf("type[4] descriptor: got %T, want ListDesc", typ.Descriptor)
		} else if desc.Element.Primitive != "u64" {
			t.Errorf("type[4] list element: got %q, want u64", desc.Element.Primitive)
		}
	}

	// Type 5: variant{circle:f64, empty}
	{
		typ := c.Types[5]
		if typ.Kind != "variant" {
			t.Errorf("type[5] kind: got %q, want variant", typ.Kind)
		}
		desc, ok := typ.Descriptor.(VariantDesc)
		if !ok {
			t.Errorf("type[5] descriptor: got %T, want VariantDesc", typ.Descriptor)
		} else if len(desc.Cases) != 2 {
			t.Errorf("type[5] variant cases: got %d, want 2", len(desc.Cases))
		} else {
			// First case: circle:f64
			if desc.Cases[0].Name != "circle" {
				t.Errorf("type[5] case[0] name: got %q, want circle", desc.Cases[0].Name)
			}
			if desc.Cases[0].Type == nil || desc.Cases[0].Type.Primitive != "f64" {
				t.Errorf("type[5] case[0] type: got %v, want f64", desc.Cases[0].Type)
			}
			// Second case: empty (no type)
			if desc.Cases[1].Name != "empty" {
				t.Errorf("type[5] case[1] name: got %q, want empty", desc.Cases[1].Name)
			}
			if desc.Cases[1].Type != nil {
				t.Errorf("type[5] case[1] should have no type, got %v", desc.Cases[1].Type)
			}
		}
	}

	// Type 6: flags[a, b, c]
	{
		typ := c.Types[6]
		if typ.Kind != "flags" {
			t.Errorf("type[6] kind: got %q, want flags", typ.Kind)
		}
		desc, ok := typ.Descriptor.(FlagsDesc)
		if !ok {
			t.Errorf("type[6] descriptor: got %T, want FlagsDesc", typ.Descriptor)
		} else if len(desc.Names) != 3 {
			t.Errorf("type[6] flag names: got %d, want 3", len(desc.Names))
		} else {
			expectedNames := []string{"a", "b", "c"}
			for i, want := range expectedNames {
				if desc.Names[i] != want {
					t.Errorf("type[6] flag[%d]: got %q, want %q", i, desc.Names[i], want)
				}
			}
		}
	}

	// Type 7: instance (simplified for M1)
	{
		typ := c.Types[7]
		if typ.Kind != "instance" {
			t.Errorf("type[7] kind: got %q, want instance", typ.Kind)
		}
		_, ok := typ.Descriptor.(InstanceDesc)
		if !ok {
			t.Errorf("type[7] descriptor: got %T, want InstanceDesc", typ.Descriptor)
		}
	}

	// Type 8: func
	{
		typ := c.Types[8]
		if typ.Kind != "func" {
			t.Errorf("type[8] kind: got %q, want func", typ.Kind)
		}
		desc, ok := typ.Descriptor.(FuncDesc)
		if !ok {
			t.Errorf("type[8] descriptor: got %T, want FuncDesc", typ.Descriptor)
		} else {
			// func with no params and no results
			if len(desc.Params) != 0 {
				t.Errorf("type[8] func params: got %d, want 0", len(desc.Params))
			}
			if desc.Results.Unnamed != nil || len(desc.Results.Named) != 0 {
				t.Errorf("type[8] func results should be empty, got %v", desc.Results)
			}
		}
	}
}

// TestHostComponent_Descriptors verifies the descriptor model against host_component.wasm.
// Ground truth (via `wasm-tools print`):
//
//	type0 instance with funcs log(string)->() and level()->(u32)
//	type1 func()->(u32)
func TestHostComponent_Descriptors(t *testing.T) {
	data, err := fixtureFS.ReadFile("testdata/host_component.wasm")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	c, err := Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	// Verify we have 2 types.
	if len(c.Types) != 2 {
		t.Fatalf("expected 2 types, got %d", len(c.Types))
	}

	// Type 0: instance
	{
		typ := c.Types[0]
		if typ.Index != 0 {
			t.Errorf("type[0] index: got %d, want 0", typ.Index)
		}
		if typ.Kind != "instance" {
			t.Errorf("type[0] kind: got %q, want instance", typ.Kind)
		}
		_, ok := typ.Descriptor.(InstanceDesc)
		if !ok {
			t.Errorf("type[0] descriptor: got %T, want InstanceDesc", typ.Descriptor)
		}
	}

	// Type 1: func()->(u32)
	{
		typ := c.Types[1]
		if typ.Index != 1 {
			t.Errorf("type[1] index: got %d, want 1", typ.Index)
		}
		if typ.Kind != "func" {
			t.Errorf("type[1] kind: got %q, want func", typ.Kind)
		}
		desc, ok := typ.Descriptor.(FuncDesc)
		if !ok {
			t.Errorf("type[1] descriptor: got %T, want FuncDesc", typ.Descriptor)
		} else {
			if len(desc.Params) != 0 {
				t.Errorf("type[1] func params: got %d, want 0", len(desc.Params))
			}
			// Result should be a single unnamed u32
			if desc.Results.Unnamed == nil || desc.Results.Unnamed.Primitive != "u32" {
				t.Errorf("type[1] func result: got %v, want u32", desc.Results.Unnamed)
			}
			if len(desc.Results.Named) != 0 {
				t.Errorf("type[1] func should have no named results, got %d", len(desc.Results.Named))
			}
		}
	}
}

// TestPrimitiveDescriptors tests that primitive types are correctly represented.
func TestPrimitiveDescriptors(t *testing.T) {
	// This would require creating a test fixture or testing through another means.
	// For now, we validate the primitive name function works.
	tests := []struct {
		b    byte
		want string
	}{
		{0x7f, "bool"},
		{0x7e, "s8"},
		{0x7d, "u8"},
		{0x7c, "s16"},
		{0x7b, "u16"},
		{0x7a, "s32"},
		{0x79, "u32"},
		{0x78, "s64"},
		{0x77, "u64"},
		{0x76, "f32"},
		{0x75, "f64"},
		{0x74, "char"},
		{0x73, "string"},
	}
	for _, tt := range tests {
		got := primName(tt.b)
		if got != tt.want {
			t.Errorf("primName(%#x): got %q, want %q", tt.b, got, tt.want)
		}
	}
}

// TestDescriptorTypeRef tests that TypeRef works correctly.
func TestDescriptorTypeRef(t *testing.T) {
	// Primitive reference
	refPrim := TypeRef{Primitive: "u32"}
	if refPrim.Primitive != "u32" || refPrim.TypeIndex != nil {
		t.Errorf("primitive ref: got {%q, %v}, want {u32, nil}", refPrim.Primitive, refPrim.TypeIndex)
	}

	// Type index reference
	idx := uint32(5)
	refIdx := TypeRef{TypeIndex: &idx}
	if refIdx.Primitive != "" || refIdx.TypeIndex == nil || *refIdx.TypeIndex != 5 {
		t.Errorf("type index ref: got {%q, %v}, want {, 5}", refIdx.Primitive, refIdx.TypeIndex)
	}
}

// TestDescriptorRecordField tests RecordField.
func TestDescriptorRecordField(t *testing.T) {
	field := RecordField{
		Name: "width",
		Type: TypeRef{Primitive: "u32"},
	}
	if field.Name != "width" {
		t.Errorf("field name: got %q, want width", field.Name)
	}
	if field.Type.Primitive != "u32" {
		t.Errorf("field type: got %q, want u32", field.Type.Primitive)
	}
}

// TestDescriptorVariantCase tests VariantCase with and without payload.
func TestDescriptorVariantCase(t *testing.T) {
	// Case with payload
	typeRef := TypeRef{Primitive: "f64"}
	caseWithPayload := VariantCase{
		Name: "circle",
		Type: &typeRef,
	}
	if caseWithPayload.Name != "circle" || caseWithPayload.Type == nil {
		t.Errorf("case with payload: Name=%q, Type=%v", caseWithPayload.Name, caseWithPayload.Type)
	}

	// Case without payload
	caseWithoutPayload := VariantCase{
		Name: "empty",
		Type: nil,
	}
	if caseWithoutPayload.Name != "empty" || caseWithoutPayload.Type != nil {
		t.Errorf("case without payload: Name=%q, Type=%v", caseWithoutPayload.Name, caseWithoutPayload.Type)
	}
}

// TestDescriptorFuncParam tests FuncParam.
func TestDescriptorFuncParam(t *testing.T) {
	param := FuncParam{
		Name: "count",
		Type: TypeRef{Primitive: "u32"},
	}
	if param.Name != "count" {
		t.Errorf("param name: got %q, want count", param.Name)
	}
	if param.Type.Primitive != "u32" {
		t.Errorf("param type: got %q, want u32", param.Type.Primitive)
	}
}

// TestDescriptorFuncResults tests FuncResults with unnamed and named results.
func TestDescriptorFuncResults(t *testing.T) {
	// Unnamed result
	unnamed := TypeRef{Primitive: "u32"}
	results := FuncResults{
		Unnamed: &unnamed,
		Named:   []FuncResult{},
	}
	if results.Unnamed == nil || results.Unnamed.Primitive != "u32" {
		t.Errorf("unnamed results: got %v, want u32", results.Unnamed)
	}
	if len(results.Named) != 0 {
		t.Errorf("named results: got %d, want 0", len(results.Named))
	}

	// Named results
	results2 := FuncResults{
		Unnamed: nil,
		Named: []FuncResult{
			{Name: "x", Type: TypeRef{Primitive: "s32"}},
			{Name: "y", Type: TypeRef{Primitive: "s32"}},
		},
	}
	if results2.Unnamed != nil {
		t.Errorf("unnamed results should be nil, got %v", results2.Unnamed)
	}
	if len(results2.Named) != 2 {
		t.Errorf("named results: got %d, want 2", len(results2.Named))
	}
	if results2.Named[0].Name != "x" {
		t.Errorf("first named result: got %q, want x", results2.Named[0].Name)
	}
}

// TestReadResourcetypeDesc_RepIsCoreType checks that a resourcetype's rep is
// decoded as a CORE valtype (i32 for 0x7f), not a component valtype (where
// 0x7f would be bool). Body = rep-valtype-byte + dtor-option.
func TestReadResourcetypeDesc_RepIsCoreType(t *testing.T) {
	// rep i32 (0x7f), no destructor (0x00)
	rd, off, err := readResourcetypeDesc([]byte{0x7f, 0x00}, 0)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rd.Rep.Primitive != "i32" {
		t.Errorf("rep: got %q, want i32 (0x7f is i32 in core wasm, not bool)", rd.Rep.Primitive)
	}
	if rd.Dtor != nil {
		t.Errorf("dtor: got %v, want none", rd.Dtor)
	}
	if off != 2 {
		t.Errorf("offset: got %d, want 2", off)
	}

	// rep i32, destructor funcidx 5 (0x01 0x05)
	rd, _, err = readResourcetypeDesc([]byte{0x7f, 0x01, 0x05}, 0)
	if err != nil {
		t.Fatalf("decode with dtor: %v", err)
	}
	if rd.Dtor == nil || *rd.Dtor != 5 {
		t.Errorf("dtor: got %v, want 5", rd.Dtor)
	}

	// invalid core rep valtype must fail loud
	if _, _, err := readResourcetypeDesc([]byte{0x72, 0x00}, 0); err == nil {
		t.Error("expected error on invalid core rep valtype 0x72")
	}
}
