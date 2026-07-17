package abi

// This file is the Go half of a differential oracle for Store/Load operations
// against the reference canonical-ABI implementation (testdata/definitions.py).
// Both halves are driven by the same battery definition: testdata/oracle_values.json.
// The Python side (testdata/gen_oracle_values.py) builds reference type objects,
// stores values, and emits testdata/oracle_values_golden.json (bytes, round-trip).
// This file builds equivalent binary.TypeDesc values from the identical JSON and
// asserts that Store/Load produce byte-identical results and round-trip correctly.
//
// To regenerate the golden file after editing oracle_values.json:
//
//	python3 internal/component/abi/testdata/gen_oracle_values.py
//
// oracle_values.json is the single contract both languages build from.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"

	bintype "github.com/samyfodil/wazy/internal/component/binary"
)

// goldenValuesEntry is the structure of one golden value entry.
type goldenValuesEntry struct {
	Ptr         uint32 `json:"ptr"`
	MemHex      string `json:"mem_hex"`
	MemLen      uint32 `json:"mem_len"`
	LoadedValue Value  `json:"loaded_value"`
}

// BumpAllocator is a deterministic allocator that matches the Python side exactly.
type BumpAllocator struct {
	current uint32
}

func NewBumpAllocator(start uint32) *BumpAllocator {
	return &BumpAllocator{current: start}
}

func (a *BumpAllocator) Alloc(origPtr, origSize, align, newSize uint32) (uint32, error) {
	if origPtr == 0 {
		// Fresh allocation
		if align > 0 {
			a.current = Align(a.current, align)
		}
		ptr := a.current
		a.current += newSize
		return ptr, nil
	} else {
		// Realloc: allocate new space
		if align > 0 {
			a.current = Align(a.current, align)
		}
		ptr := a.current
		a.current += newSize
		return ptr, nil
	}
}

// jsonToValue converts a generic JSON value to a properly typed Go Value based on the type.
func jsonToValue(jsonVal any, t bintype.TypeDesc, resolve Resolver) (Value, error) {
	switch desc := t.(type) {
	case bintype.PrimitiveDesc:
		return jsonToPrimitive(jsonVal, desc.Prim)

	case bintype.ListDesc:
		elemType, err := resolveType(&desc.Element, resolve)
		if err != nil {
			return nil, err
		}
		jsonList, ok := jsonVal.([]any)
		if !ok {
			return nil, fmt.Errorf("jsonToValue: expected list, got %T", jsonVal)
		}
		result := make([]Value, len(jsonList))
		for i, elem := range jsonList {
			v, err := jsonToValue(elem, elemType, resolve)
			if err != nil {
				return nil, fmt.Errorf("jsonToValue list[%d]: %w", i, err)
			}
			result[i] = v
		}
		return result, nil

	case bintype.RecordDesc:
		// Records can be represented as either arrays (field order) or maps (field names)
		result := make([]Value, len(desc.Fields))

		// Try as array first (field order)
		if jsonArr, ok := jsonVal.([]any); ok {
			if len(jsonArr) != len(desc.Fields) {
				return nil, fmt.Errorf("jsonToValue: record array len mismatch: got %d, want %d", len(jsonArr), len(desc.Fields))
			}
			for i, field := range desc.Fields {
				fieldType, err := resolveType(&field.Type, resolve)
				if err != nil {
					return nil, fmt.Errorf("jsonToValue record %s: %w", field.Name, err)
				}
				v, err := jsonToValue(jsonArr[i], fieldType, resolve)
				if err != nil {
					return nil, fmt.Errorf("jsonToValue record %s: %w", field.Name, err)
				}
				result[i] = v
			}
			return result, nil
		}

		// Try as map (field names)
		if jsonRec, ok := jsonVal.(map[string]any); ok {
			for i, field := range desc.Fields {
				fieldType, err := resolveType(&field.Type, resolve)
				if err != nil {
					return nil, fmt.Errorf("jsonToValue record %s: %w", field.Name, err)
				}
				fieldVal, ok := jsonRec[field.Name]
				if !ok {
					return nil, fmt.Errorf("jsonToValue record: missing field %s", field.Name)
				}
				v, err := jsonToValue(fieldVal, fieldType, resolve)
				if err != nil {
					return nil, fmt.Errorf("jsonToValue record %s: %w", field.Name, err)
				}
				result[i] = v
			}
			return result, nil
		}

		return nil, fmt.Errorf("jsonToValue: expected record array or map, got %T", jsonVal)

	case bintype.TupleDesc:
		jsonTuple, ok := jsonVal.([]any)
		if !ok {
			return nil, fmt.Errorf("jsonToValue: expected tuple, got %T", jsonVal)
		}
		if len(jsonTuple) != len(desc.Elements) {
			return nil, fmt.Errorf("jsonToValue: tuple len mismatch: got %d, want %d", len(jsonTuple), len(desc.Elements))
		}
		result := make([]Value, len(desc.Elements))
		for i, elemRef := range desc.Elements {
			elemType, err := resolveType(&elemRef, resolve)
			if err != nil {
				return nil, fmt.Errorf("jsonToValue tuple[%d]: %w", i, err)
			}
			v, err := jsonToValue(jsonTuple[i], elemType, resolve)
			if err != nil {
				return nil, fmt.Errorf("jsonToValue tuple[%d]: %w", i, err)
			}
			result[i] = v
		}
		return result, nil

	case bintype.VariantDesc:
		jsonVar, ok := jsonVal.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("jsonToValue: expected variant, got %T", jsonVal)
		}

		// Variant JSON format: {"disc": N, "payload": ...}
		discVal, ok := jsonVar["disc"]
		if !ok {
			return nil, fmt.Errorf("jsonToValue variant: missing disc")
		}
		discNum, ok := discVal.(float64)
		if !ok {
			return nil, fmt.Errorf("jsonToValue variant: disc not number")
		}
		disc := uint32(discNum)

		if disc >= uint32(len(desc.Cases)) {
			return nil, fmt.Errorf("jsonToValue variant: disc %d out of range [0,%d)", disc, len(desc.Cases))
		}

		payloadVal, hasPayload := jsonVar["payload"]
		var payload Value
		if hasPayload && payloadVal != nil && desc.Cases[disc].Type != nil {
			caseType, err := resolveType(desc.Cases[disc].Type, resolve)
			if err != nil {
				return nil, fmt.Errorf("jsonToValue variant case %d: %w", disc, err)
			}
			p, err := jsonToValue(payloadVal, caseType, resolve)
			if err != nil {
				return nil, fmt.Errorf("jsonToValue variant case %d: %w", disc, err)
			}
			payload = p
		}
		return VariantValue{Disc: disc, Payload: payload}, nil

	case bintype.FlagsDesc:
		jsonNum, ok := jsonVal.(float64)
		if !ok {
			return nil, fmt.Errorf("jsonToValue: expected flags number, got %T", jsonVal)
		}
		return uint32(jsonNum), nil

	case bintype.EnumDesc:
		jsonNum, ok := jsonVal.(float64)
		if !ok {
			return nil, fmt.Errorf("jsonToValue: expected enum number, got %T", jsonVal)
		}
		return uint32(jsonNum), nil

	case bintype.OptionDesc:
		if jsonVal == nil {
			return nil, nil
		}
		elemType, err := resolveType(&desc.Element, resolve)
		if err != nil {
			return nil, err
		}
		v, err := jsonToValue(jsonVal, elemType, resolve)
		if err != nil {
			return nil, fmt.Errorf("jsonToValue option: %w", err)
		}
		return v, nil

	case bintype.ResultDesc:
		jsonRes, ok := jsonVal.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("jsonToValue: expected result, got %T", jsonVal)
		}
		isErr, hasErr := jsonRes["isErr"]
		payloadVal, hasPayload := jsonRes["payload"]
		if !hasErr {
			return nil, fmt.Errorf("jsonToValue result: missing isErr")
		}
		isErrBool, ok := isErr.(bool)
		if !ok {
			return nil, fmt.Errorf("jsonToValue result: isErr not bool")
		}
		var payload Value
		if hasPayload && payloadVal != nil {
			var t bintype.TypeDesc
			var err error
			if isErrBool && desc.Err != nil {
				t, err = resolveType(desc.Err, resolve)
			} else if !isErrBool && desc.Ok != nil {
				t, err = resolveType(desc.Ok, resolve)
			}
			if err != nil {
				return nil, fmt.Errorf("jsonToValue result: %w", err)
			}
			if t != nil {
				p, err := jsonToValue(payloadVal, t, resolve)
				if err != nil {
					return nil, fmt.Errorf("jsonToValue result: %w", err)
				}
				payload = p
			}
		}
		return ResultValue{IsErr: isErrBool, Payload: payload}, nil

	case bintype.OwnDesc, bintype.BorrowDesc:
		jsonNum, ok := jsonVal.(float64)
		if !ok {
			return nil, fmt.Errorf("jsonToValue: expected handle number, got %T", jsonVal)
		}
		return uint32(jsonNum), nil

	default:
		return nil, fmt.Errorf("jsonToValue: unsupported type %T", t)
	}
}

func jsonToPrimitive(jsonVal any, prim string) (Value, error) {
	switch prim {
	case "bool":
		b, ok := jsonVal.(bool)
		if !ok {
			return nil, fmt.Errorf("jsonToPrimitive: expected bool, got %T", jsonVal)
		}
		return b, nil

	case "u8", "u16", "u32":
		n, ok := jsonVal.(float64)
		if !ok {
			return nil, fmt.Errorf("jsonToPrimitive: expected number, got %T", jsonVal)
		}
		return uint32(n), nil

	case "s8", "s16", "s32":
		n, ok := jsonVal.(float64)
		if !ok {
			return nil, fmt.Errorf("jsonToPrimitive: expected number, got %T", jsonVal)
		}
		return int32(n), nil

	case "u64":
		n, ok := jsonVal.(float64)
		if !ok {
			return nil, fmt.Errorf("jsonToPrimitive: expected number, got %T", jsonVal)
		}
		return uint64(n), nil

	case "s64":
		n, ok := jsonVal.(float64)
		if !ok {
			return nil, fmt.Errorf("jsonToPrimitive: expected number, got %T", jsonVal)
		}
		return int64(n), nil

	case "f32":
		n, ok := jsonVal.(float64)
		if !ok {
			return nil, fmt.Errorf("jsonToPrimitive: expected number, got %T", jsonVal)
		}
		return float32(n), nil

	case "f64":
		n, ok := jsonVal.(float64)
		if !ok {
			return nil, fmt.Errorf("jsonToPrimitive: expected number, got %T", jsonVal)
		}
		return n, nil

	case "char":
		n, ok := jsonVal.(float64)
		if !ok {
			return nil, fmt.Errorf("jsonToPrimitive: expected number, got %T", jsonVal)
		}
		return rune(n), nil

	case "string":
		s, ok := jsonVal.(string)
		if !ok {
			return nil, fmt.Errorf("jsonToPrimitive: expected string, got %T", jsonVal)
		}
		return s, nil

	default:
		return nil, fmt.Errorf("jsonToPrimitive: unknown primitive %s", prim)
	}
}

func TestValuesOracleRoundTrip(t *testing.T) {
	typesData, err := oracleTestdata.ReadFile("testdata/oracle_values.json")
	if err != nil {
		t.Fatalf("reading oracle_values.json: %v", err)
	}

	goldenData, err := oracleTestdata.ReadFile("testdata/oracle_values_golden.json")
	if err != nil {
		t.Fatalf("reading oracle_values_golden.json: %v", err)
	}

	var valuesFile struct {
		Values []struct {
			Name  string          `json:"name"`
			Type  json.RawMessage `json:"type"`
			Value json.RawMessage `json:"value"`
		} `json:"values"`
	}
	if err := json.Unmarshal(typesData, &valuesFile); err != nil {
		t.Fatalf("parsing oracle_values.json: %v", err)
	}

	var golden map[string]goldenValuesEntry
	if err := json.Unmarshal(goldenData, &golden); err != nil {
		t.Fatalf("parsing oracle_values_golden.json: %v", err)
	}

	if len(valuesFile.Values) == 0 {
		t.Fatal("oracle_values.json battery is empty")
	}

	nameToIndex := make(map[string]uint32, len(valuesFile.Values))
	for i, entry := range valuesFile.Values {
		nameToIndex[entry.Name] = uint32(i)
	}

	resolveMap := make(map[uint32]bintype.TypeDesc, len(valuesFile.Values))
	resolve := func(idx uint32) bintype.TypeDesc {
		desc, ok := resolveMap[idx]
		if !ok {
			panic(fmt.Sprintf("oracle: unresolved type index %d", idx))
		}
		return desc
	}

	for i, entry := range valuesFile.Values {
		idx := uint32(i)
		name := entry.Name

		want, ok := golden[name]
		if !ok {
			t.Errorf("%s: no golden entry (did you run gen_oracle_values.py?)", name)
			continue
		}

		// Build type descriptor
		typeDesc, err := buildTypeDesc(entry.Type, nameToIndex)
		if err != nil {
			t.Fatalf("%s: building type desc: %v", name, err)
		}
		resolveMap[idx] = typeDesc

		// Parse value JSON and convert to typed value
		var jsonValue any
		if err := json.Unmarshal(entry.Value, &jsonValue); err != nil {
			t.Fatalf("%s: parsing value: %v", name, err)
		}

		value, err := jsonToValue(jsonValue, typeDesc, resolve)
		if err != nil {
			t.Fatalf("%s: converting value: %v", name, err)
		}

		t.Run(name, func(t *testing.T) {
			// Create memory and allocator matching Python side
			mem := make([]byte, 65536)
			alloc := NewBumpAllocator(1024)

			// Store the value
			if err := Store(mem, 1024, typeDesc, value, resolve, ReallocFunc(alloc.Alloc)); err != nil {
				t.Fatalf("Store: %v", err)
			}

			// Check that stored bytes match golden
			endPtr := alloc.current
			storedbytes := mem[1024:endPtr]
			expectedBytes, err := hex.DecodeString(want.MemHex)
			if err != nil {
				t.Fatalf("decoding golden mem_hex: %v", err)
			}

			if !bytes.Equal(storedbytes, expectedBytes) {
				t.Errorf("Store bytes mismatch\ngot:\n%s\nwant:\n%s",
					hex.EncodeToString(storedbytes),
					want.MemHex)
			}

			// Load the value back
			loaded, err := Load(mem, 1024, typeDesc, resolve)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			// Compare loaded value (after JSON round-trip to normalize)
			loadedJSON, err := json.Marshal(loaded)
			if err != nil {
				t.Fatalf("marshaling loaded value: %v", err)
			}
			expectedJSON, err := json.Marshal(want.LoadedValue)
			if err != nil {
				t.Fatalf("marshaling golden loaded value: %v", err)
			}

			if !bytes.Equal(loadedJSON, expectedJSON) {
				t.Errorf("Load value mismatch\ngot:\n%s\nwant:\n%s",
					string(loadedJSON), string(expectedJSON))
			}
		})
	}
}
