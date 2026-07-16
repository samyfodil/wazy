package abi

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"reflect"
	"strconv"
	"testing"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// This file is the Go half of a differential oracle against the reference
// canonical-ABI implementation (testdata/definitions.py). It verifies that
// LowerFlat and LiftFlat produce the exact same CoreValue representations
// (Kind + Bits, not just count) as the Python reference implementation, AND
// that the linear-memory bytes actually written during lowering match
// byte-for-byte.
//
// The mem_hex check is what makes this oracle meaningful for string/list:
// a lowerFlatString/lowerFlatList that returns a placeholder (0,0) instead
// of doing the real realloc+copy would still need to be caught even if a
// weaker oracle only checked core-value *count*. Comparing the full
// {Kind,Bits} pair catches the zero ptr, and comparing mem_hex catches a
// stub that returns a plausible-looking pointer without ever writing the
// payload bytes.
//
// Both halves use the exact same deterministic bump allocator (starting at
// mem_base=1024, see bumpAllocator below and BumpAllocator in
// gen_oracle_flat.py) driven by the exact same sequence of realloc calls, so
// the pointers embedded in core_vals/mem_hex are reproducible byte-for-byte
// across languages -- not just "some pointer", but *the* pointer.
//
// The oracle is defined by:
// - testdata/oracle_flat.json: type and value definitions
// - testdata/oracle_flat_golden.json: expected LowerFlat results from Python
//
// To regenerate the golden file:
//   python3 internal/component/abi/testdata/gen_oracle_flat.py

type flatOracleGoldenEntry struct {
	Name     string                `json:"name"`
	Type     string                `json:"type"`
	CoreVals []flatOracleCoreValue `json:"core_vals"`
	MemBase  uint32                `json:"mem_base"`
	MemHex   string                `json:"mem_hex"`
}

type flatOracleCoreValue struct {
	Kind string `json:"kind"` // "i32", "i64", "f32", "f64"
	Bits uint64 `json:"bits"` // the bit pattern
}

func (v flatOracleCoreValue) toCoreValue() CoreValue {
	switch v.Kind {
	case "i32":
		return NewCoreValueI32(uint32(v.Bits))
	case "i64":
		return NewCoreValueI64(v.Bits)
	case "f32":
		return NewCoreValueF32(math.Float32frombits(uint32(v.Bits)))
	case "f64":
		return NewCoreValueF64(math.Float64frombits(v.Bits))
	default:
		panic(fmt.Sprintf("unknown core value kind: %s", v.Kind))
	}
}

func loadFlatOracleGolden(t *testing.T) []flatOracleGoldenEntry {
	t.Helper()
	data, err := os.ReadFile("testdata/oracle_flat_golden.json")
	if err != nil {
		t.Fatalf("failed to load oracle_flat_golden.json: %v", err)
	}

	var entries []flatOracleGoldenEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("failed to parse oracle_flat_golden.json: %v", err)
	}

	return entries
}

// flatOracleBattery holds everything derived from oracle_flat.json: the
// built binary.TypeDesc for every named type (available to every other type
// via "ref", exactly like oracle_test.go's battery), a Resolver closure over
// that table, and the raw JSON values keyed by "type:name" for building Go
// Values out of.
type flatOracleBattery struct {
	descByName map[string]binary.TypeDesc
	resolve    Resolver
	valueMap   map[string]any
}

func loadFlatOracleBattery(t *testing.T) flatOracleBattery {
	t.Helper()
	data, err := os.ReadFile("testdata/oracle_flat.json")
	if err != nil {
		t.Fatalf("failed to load oracle_flat.json: %v", err)
	}

	var root struct {
		Types []struct {
			Name string          `json:"name"`
			Type json.RawMessage `json:"type"`
		} `json:"types"`
		Values []struct {
			Type  string `json:"type"`
			Name  string `json:"name"`
			Value any    `json:"value"`
		} `json:"values"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("failed to parse oracle_flat.json: %v", err)
	}

	// Assign every battery entry a stable index (its position) so "ref"
	// nodes can become TypeRef{TypeIndex}. The battery only contains
	// backward references, so a single top-to-bottom pass both builds and
	// resolves (same approach as oracle_test.go's TestOracleAgainstReference).
	nameToIndex := make(map[string]uint32, len(root.Types))
	for i, te := range root.Types {
		nameToIndex[te.Name] = uint32(i)
	}

	typeDescs := make([]binary.TypeDesc, len(root.Types))
	descByName := make(map[string]binary.TypeDesc, len(root.Types))
	resolve := func(idx uint32) binary.TypeDesc {
		if int(idx) >= len(typeDescs) {
			panic(fmt.Sprintf("flat oracle: unresolved type index %d", idx))
		}
		return typeDescs[idx]
	}

	for i, te := range root.Types {
		desc, err := specToFlatTypeDesc(te.Type, nameToIndex)
		if err != nil {
			t.Fatalf("building type %q: %v", te.Name, err)
		}
		typeDescs[i] = desc
		descByName[te.Name] = desc
	}

	valueMap := make(map[string]any, len(root.Values))
	for _, ve := range root.Values {
		valueMap[fmt.Sprintf("%s:%s", ve.Type, ve.Name)] = ve.Value
	}

	return flatOracleBattery{descByName: descByName, resolve: resolve, valueMap: valueMap}
}

// bumpAllocator is a deterministic Realloc: every call is treated as a fresh
// allocation (like gen_oracle_flat.py's BumpAllocator, and matching what
// this oracle's test data actually needs -- none of it grows an existing
// allocation), so the address handed out depends only on the *order* of
// realloc calls, which is fixed by the value being lowered and therefore
// identical between this Go test and the Python reference.
type bumpAllocator struct {
	next uint32
}

func newBumpAllocator(base uint32) *bumpAllocator {
	return &bumpAllocator{next: base}
}

func (b *bumpAllocator) realloc(_, _, align, newSize uint32) (uint32, error) {
	ptr := Align(b.next, align)
	b.next = ptr + newSize
	return ptr, nil
}

// TestFlatOracleLowerFlat verifies that LowerFlat produces the exact
// CoreValue representations *and* writes the exact memory bytes the Python
// oracle does, for every entry in the battery.
func TestFlatOracleLowerFlat(t *testing.T) {
	goldenEntries := loadFlatOracleGolden(t)
	battery := loadFlatOracleBattery(t)

	// Entries whose lowering must allocate (i.e. must produce a non-zero
	// pointer) -- a lowerFlatString/lowerFlatList stub returning (0,0)
	// placeholders would fail these even before the mem_hex check kicks in.
	mustAllocate := map[string]bool{
		"string_hello":           true,
		"list_u32_vals":          true,
		"list_string_vals":       true,
		"record_string_u32_vals": true,
		"option_string_some":     true,
		"result_string_u32_ok":   true,
		"tuple_list_u32_vals":    true,
	}

	successCount := 0
	for _, entry := range goldenEntries {
		t.Run(entry.Name, func(t *testing.T) {
			desc, ok := battery.descByName[entry.Type]
			if !ok {
				t.Fatalf("no type %q in battery", entry.Type)
			}

			valueKey := fmt.Sprintf("%s:%s", entry.Type, entry.Name)
			rawValue, ok := battery.valueMap[valueKey]
			if !ok {
				t.Fatalf("no value %q in battery", valueKey)
			}
			v, err := convertTestValue(rawValue, desc, battery.resolve)
			if err != nil {
				t.Fatalf("convert error: %v", err)
			}

			// Fresh memory + fresh deterministic bump allocator, exactly
			// mirroring gen_oracle_flat.py's per-value make_context().
			mem := make([]byte, 65536)
			alloc := newBumpAllocator(entry.MemBase)

			result, err := LowerFlat(v, desc, battery.resolve, ReallocFunc(alloc.realloc), mem)
			if err != nil {
				t.Fatalf("LowerFlat: %v", err)
			}

			if len(result) != len(entry.CoreVals) {
				t.Fatalf("expected %d core values, got %d", len(entry.CoreVals), len(result))
			}
			for i, expected := range entry.CoreVals {
				actual := result[i]
				if actual.Kind != expected.Kind || actual.Bits != expected.Bits {
					t.Errorf("core_vals[%d]: got {%s 0x%x}, want {%s 0x%x}",
						i, actual.Kind, actual.Bits, expected.Kind, expected.Bits)
				}
			}

			// Compare the memory actually written: [MemBase, watermark).
			wantMem, err := hex.DecodeString(entry.MemHex)
			if err != nil {
				t.Fatalf("bad golden mem_hex: %v", err)
			}
			gotMem := mem[entry.MemBase : entry.MemBase+uint32(len(wantMem))]
			if !reflect.DeepEqual(gotMem, wantMem) {
				t.Errorf("mem[%d:%d] = %x, want %x", entry.MemBase, entry.MemBase+uint32(len(wantMem)), gotMem, wantMem)
			}
			// Nothing should have been written past the golden watermark:
			// a stub that scribbles extra bytes (or a real bug that
			// over-allocates) would otherwise slip through undetected.
			if alloc.next != entry.MemBase+uint32(len(wantMem)) {
				t.Errorf("bump allocator ended at %d, want %d (mem_hex length mismatch)", alloc.next, entry.MemBase+uint32(len(wantMem)))
			}

			if mustAllocate[entry.Name] {
				foundNonZeroPtr := false
				for _, cv := range result {
					if cv.Kind == "i32" && cv.Bits != 0 {
						foundNonZeroPtr = true
						break
					}
				}
				if !foundNonZeroPtr {
					t.Errorf("%s: expected a non-zero pointer among core_vals (proof real allocation happened), got %+v", entry.Name, result)
				}
				if len(wantMem) == 0 {
					t.Errorf("%s: expected non-empty golden mem_hex (proof the battery exercises string/list storage)", entry.Name)
				}
			}

			if !t.Failed() {
				successCount++
			}
		})
	}

	t.Logf("Passed %d/%d tests", successCount, len(goldenEntries))
}

// TestFlatOracleRoundTrip verifies that LiftFlat correctly reconstructs the
// original Go Value from the golden core_vals + memory bytes emitted by
// LowerFlat -- i.e. that liftFlatString/liftFlatList (and everything that
// contains them) actually read the data back out of memory, rather than
// returning "" / []Value{} placeholders regardless of what's there.
func TestFlatOracleRoundTrip(t *testing.T) {
	goldenEntries := loadFlatOracleGolden(t)
	battery := loadFlatOracleBattery(t)

	for _, entry := range goldenEntries {
		t.Run(entry.Name, func(t *testing.T) {
			desc, ok := battery.descByName[entry.Type]
			if !ok {
				t.Fatalf("no type %q in battery", entry.Type)
			}

			valueKey := fmt.Sprintf("%s:%s", entry.Type, entry.Name)
			rawValue, ok := battery.valueMap[valueKey]
			if !ok {
				t.Fatalf("no value %q in battery", valueKey)
			}
			want, err := convertTestValue(rawValue, desc, battery.resolve)
			if err != nil {
				t.Fatalf("convert error: %v", err)
			}

			// Reconstruct memory from the golden bytes at MemBase.
			wantMem, err := hex.DecodeString(entry.MemHex)
			if err != nil {
				t.Fatalf("bad golden mem_hex: %v", err)
			}
			mem := make([]byte, 65536)
			copy(mem[entry.MemBase:], wantMem)

			vals := make([]CoreValue, len(entry.CoreVals))
			for i, cv := range entry.CoreVals {
				vals[i] = cv.toCoreValue()
			}

			got, err := LiftFlat(vals, desc, battery.resolve, mem)
			if err != nil {
				t.Fatalf("LiftFlat: %v", err)
			}

			if !reflect.DeepEqual(got, want) {
				t.Errorf("LiftFlat round-trip = %#v, want %#v", got, want)
			}
		})
	}
}

// specToFlatTypeDesc converts a TypeSpec JSON to binary.TypeDesc for flat testing.
func specToFlatTypeDesc(spec json.RawMessage, nameToIndex map[string]uint32) (binary.TypeDesc, error) {
	var node struct {
		Kind   string            `json:"kind"`
		Prim   string            `json:"prim"`
		Fields []json.RawMessage `json:"fields"`
		Cases  []json.RawMessage `json:"cases"`
		Elems  []json.RawMessage `json:"elems"`
		Elem   json.RawMessage   `json:"elem"`
		Names  []string          `json:"names"`
		Ok     json.RawMessage   `json:"ok"`
		Err    json.RawMessage   `json:"err"`
	}

	if err := json.Unmarshal(spec, &node); err != nil {
		return nil, err
	}

	switch node.Kind {
	case "primitive":
		return binary.PrimitiveDesc{Prim: node.Prim}, nil

	case "list":
		elemRef, err := specToFlatTypeRef(node.Elem, nameToIndex)
		if err != nil {
			return nil, fmt.Errorf("list element: %w", err)
		}
		return binary.ListDesc{Element: elemRef}, nil

	case "record":
		var fields []binary.RecordField
		for _, fieldSpec := range node.Fields {
			var fieldNode struct {
				Name string          `json:"name"`
				Type json.RawMessage `json:"type"`
			}
			if err := json.Unmarshal(fieldSpec, &fieldNode); err != nil {
				return nil, err
			}
			fieldType, err := specToFlatTypeRef(fieldNode.Type, nameToIndex)
			if err != nil {
				return nil, err
			}
			fields = append(fields, binary.RecordField{Name: fieldNode.Name, Type: fieldType})
		}
		return binary.RecordDesc{Fields: fields}, nil

	case "variant":
		var cases []binary.VariantCase
		for _, caseSpec := range node.Cases {
			var caseNode struct {
				Name string          `json:"name"`
				Type json.RawMessage `json:"type"`
			}
			if err := json.Unmarshal(caseSpec, &caseNode); err != nil {
				return nil, err
			}
			var caseType *binary.TypeRef
			if len(caseNode.Type) > 0 {
				ct, err := specToFlatTypeRef(caseNode.Type, nameToIndex)
				if err != nil {
					return nil, err
				}
				caseType = &ct
			}
			cases = append(cases, binary.VariantCase{Name: caseNode.Name, Type: caseType})
		}
		return binary.VariantDesc{Cases: cases}, nil

	case "option":
		elemType, err := specToFlatTypeRef(node.Elem, nameToIndex)
		if err != nil {
			return nil, err
		}
		return binary.OptionDesc{Element: elemType}, nil

	case "result":
		var ok, errRef *binary.TypeRef
		if len(node.Ok) > 0 {
			okType, e := specToFlatTypeRef(node.Ok, nameToIndex)
			if e != nil {
				return nil, e
			}
			ok = &okType
		}
		if len(node.Err) > 0 {
			et, e := specToFlatTypeRef(node.Err, nameToIndex)
			if e != nil {
				return nil, e
			}
			errRef = &et
		}
		return binary.ResultDesc{Ok: ok, Err: errRef}, nil

	case "tuple":
		var elems []binary.TypeRef
		for _, elemSpec := range node.Elems {
			elem, err := specToFlatTypeRef(elemSpec, nameToIndex)
			if err != nil {
				return nil, err
			}
			elems = append(elems, elem)
		}
		return binary.TupleDesc{Elements: elems}, nil

	case "flags":
		return binary.FlagsDesc{Names: node.Names}, nil

	case "enum":
		return binary.EnumDesc{Cases: node.Names}, nil

	default:
		return nil, fmt.Errorf("unknown type kind: %s", node.Kind)
	}
}

// specToFlatTypeRef converts a TypeSpec JSON to binary.TypeRef. Only
// "primitive" and "ref" are valid here (record fields, list elements, tuple
// elements, option/result payloads): the binary format itself only allows a
// primitive or a type-table index in this position, never an inline
// composite -- a composite like list<u32> must be declared as its own named
// battery entry and referenced with {"kind":"ref","name":...}.
func specToFlatTypeRef(spec json.RawMessage, nameToIndex map[string]uint32) (binary.TypeRef, error) {
	var node struct {
		Kind string `json:"kind"`
		Prim string `json:"prim"`
		Name string `json:"name"`
	}

	if err := json.Unmarshal(spec, &node); err != nil {
		return binary.TypeRef{}, err
	}

	switch node.Kind {
	case "primitive":
		return binary.TypeRef{Primitive: node.Prim}, nil
	case "ref":
		idx, ok := nameToIndex[node.Name]
		if !ok {
			return binary.TypeRef{}, fmt.Errorf("ref %q: no such battery entry (must be declared earlier)", node.Name)
		}
		return binary.TypeRef{TypeIndex: &idx}, nil
	default:
		return binary.TypeRef{}, fmt.Errorf("expected primitive or ref in TypeRef, got %s", node.Kind)
	}
}

// convertTestValue converts a JSON test value to a Go Value.
func convertTestValue(rawValue any, t binary.TypeDesc, resolve Resolver) (Value, error) {
	switch desc := t.(type) {
	case binary.PrimitiveDesc:
		return convertTestPrimitive(rawValue, desc.Prim)

	case binary.ListDesc:
		list, ok := rawValue.([]any)
		if !ok {
			return nil, fmt.Errorf("cannot convert value %v to list", rawValue)
		}
		elemType, err := resolveType(&desc.Element, resolve)
		if err != nil {
			return nil, err
		}
		result := make([]Value, len(list))
		for i, val := range list {
			v, err := convertTestValue(val, elemType, resolve)
			if err != nil {
				return nil, err
			}
			result[i] = v
		}
		return result, nil

	case binary.RecordDesc:
		if list, ok := rawValue.([]any); ok {
			result := make([]Value, len(desc.Fields))
			for i, val := range list {
				if i >= len(desc.Fields) {
					break
				}
				fieldType, err := resolveType(&desc.Fields[i].Type, resolve)
				if err != nil {
					return nil, err
				}
				v, err := convertTestValue(val, fieldType, resolve)
				if err != nil {
					return nil, err
				}
				result[i] = v
			}
			return result, nil
		}

	case binary.VariantDesc:
		if m, ok := rawValue.(map[string]any); ok {
			for i, c := range desc.Cases {
				if val, has := m[c.Name]; has {
					var payload Value
					if c.Type != nil {
						caseType, err := resolveType(c.Type, resolve)
						if err != nil {
							return nil, err
						}
						p, err := convertTestValue(val, caseType, resolve)
						if err != nil {
							return nil, err
						}
						payload = p
					}
					return VariantValue{Disc: uint32(i), Payload: payload}, nil
				}
			}
		}

	case binary.TupleDesc:
		if list, ok := rawValue.([]any); ok {
			result := make([]Value, len(desc.Elements))
			for i, val := range list {
				if i >= len(desc.Elements) {
					break
				}
				elemType, err := resolveType(&desc.Elements[i], resolve)
				if err != nil {
					return nil, err
				}
				v, err := convertTestValue(val, elemType, resolve)
				if err != nil {
					return nil, err
				}
				result[i] = v
			}
			return result, nil
		}

	case binary.OptionDesc:
		if rawValue == nil {
			return nil, nil
		}
		elemType, err := resolveType(&desc.Element, resolve)
		if err != nil {
			return nil, err
		}
		return convertTestValue(rawValue, elemType, resolve)

	case binary.ResultDesc:
		if m, ok := rawValue.(map[string]any); ok {
			if val, hasOk := m["ok"]; hasOk {
				var payload Value
				if desc.Ok != nil {
					okType, err := resolveType(desc.Ok, resolve)
					if err != nil {
						return nil, err
					}
					p, err := convertTestValue(val, okType, resolve)
					if err != nil {
						return nil, err
					}
					payload = p
				}
				return ResultValue{IsErr: false, Payload: payload}, nil
			}
			if val, hasErr := m["err"]; hasErr {
				var payload Value
				if desc.Err != nil {
					errType, err := resolveType(desc.Err, resolve)
					if err != nil {
						return nil, err
					}
					p, err := convertTestValue(val, errType, resolve)
					if err != nil {
						return nil, err
					}
					payload = p
				}
				return ResultValue{IsErr: true, Payload: payload}, nil
			}
		}

	case binary.FlagsDesc:
		if v, ok := rawValue.(float64); ok {
			return uint32(v), nil
		}

	case binary.EnumDesc:
		if v, ok := rawValue.(float64); ok {
			return uint32(v), nil
		}
	}

	return nil, fmt.Errorf("cannot convert value %v to type %T", rawValue, t)
}

// convertTestPrimitive converts a JSON value to a primitive Go value.
func convertTestPrimitive(rawValue any, prim string) (Value, error) {
	switch prim {
	case "bool":
		if v, ok := rawValue.(bool); ok {
			return v, nil
		}

	case "u8", "u16", "u32":
		if v, ok := rawValue.(float64); ok {
			return uint32(v), nil
		}

	case "s8", "s16", "s32":
		if v, ok := rawValue.(float64); ok {
			return int32(v), nil
		}

	case "u64":
		// A JSON string carries an exact value the float64 form would round
		// (u64 > 2^53); ParseUint with base 0 accepts "0x..."/decimal.
		if s, ok := rawValue.(string); ok {
			u, err := strconv.ParseUint(s, 0, 64)
			if err != nil {
				return nil, fmt.Errorf("u64 %q: %w", s, err)
			}
			return u, nil
		}
		if v, ok := rawValue.(float64); ok {
			return uint64(v), nil
		}

	case "s64":
		if s, ok := rawValue.(string); ok {
			i, err := strconv.ParseInt(s, 0, 64)
			if err != nil {
				return nil, fmt.Errorf("s64 %q: %w", s, err)
			}
			return i, nil
		}
		if v, ok := rawValue.(float64); ok {
			return int64(v), nil
		}

	case "f32":
		if v, ok := rawValue.(float64); ok {
			return float32(v), nil
		}

	case "f64":
		if v, ok := rawValue.(float64); ok {
			return v, nil
		}

	case "char":
		if v, ok := rawValue.(float64); ok {
			return rune(v), nil
		}

	case "string":
		if v, ok := rawValue.(string); ok {
			return v, nil
		}
	}

	return nil, fmt.Errorf("cannot convert value %v to primitive %s", rawValue, prim)
}
