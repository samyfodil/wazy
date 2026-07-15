package abi

// This file is the Go half of a differential oracle against the reference
// canonical-ABI implementation (testdata/definitions.py, vendored from
// https://github.com/WebAssembly/component-model). Both halves are driven by
// the SAME battery definition, testdata/oracle_types.json: the Python side
// (testdata/gen_oracle.py) builds the reference type objects from it and
// emits testdata/oracle_golden.json; this file builds the equivalent
// binary.TypeDesc values from the identical JSON and asserts that
// Size/Alignment/Flatten/FlattenFunc agree exactly with that golden file.
//
// To regenerate the golden file after editing oracle_types.json or updating
// the vendored definitions.py:
//
//	python3 internal/component/abi/testdata/gen_oracle.py
//
// oracle_types.json is the single contract both languages build from, so
// there is no risk of the two batteries drifting apart independently.

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// ------- oracle_types.json schema -------

type oracleTypesFile struct {
	Types []oracleTypeEntry `json:"types"`
}

type oracleTypeEntry struct {
	Name string          `json:"name"`
	Type json.RawMessage `json:"type"`
}

// typeSpecNode is a generic decode target covering every TypeSpec "kind".
// Not all fields apply to every kind; see buildTypeDesc/specToTypeRef.
type typeSpecNode struct {
	Kind    string            `json:"kind"`
	Prim    string            `json:"prim"`
	Fields  []fieldSpecNode   `json:"fields"`
	Cases   json.RawMessage   `json:"cases"` // []caseSpecNode (variant) or []string (enum)
	Elem    json.RawMessage   `json:"elem"`
	Elems   []json.RawMessage `json:"elems"`
	Names   []string          `json:"names"`
	Ok      json.RawMessage   `json:"ok"`
	Err     json.RawMessage   `json:"err"`
	Name    string            `json:"name"` // for kind == "ref"
	Params  []fieldSpecNode   `json:"params"`
	Results funcResultsNode   `json:"results"`
}

type fieldSpecNode struct {
	Name string          `json:"name"`
	Type json.RawMessage `json:"type"`
}

type caseSpecNode struct {
	Name string          `json:"name"`
	Type json.RawMessage `json:"type"`
}

type funcResultsNode struct {
	Unnamed json.RawMessage `json:"unnamed"`
	Named   []fieldSpecNode `json:"named"`
}

// ------- oracle_golden.json schema -------

type goldenEntry struct {
	Kind      string          `json:"kind"`
	Size      uint32          `json:"size"`
	Alignment uint32          `json:"alignment"`
	Flatten   []string        `json:"flatten"`
	Lift      *goldenFuncFlat `json:"lift"`
	Lower     *goldenFuncFlat `json:"lower"`
}

type goldenFuncFlat struct {
	Params  []string `json:"params"`
	Results []string `json:"results"`
}

// ------- JSON -> binary.TypeDesc construction -------

// isJSONNull reports whether a raw JSON value is absent or explicitly null.
func isJSONNull(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

// specToTypeRef converts a TypeSpec JSON node into a binary.TypeRef. Only
// "primitive" and "ref" are valid in TypeRef position (record fields, list
// elements, tuple elements, variant/option/result payloads, func
// params/results): the binary format itself only allows a primitive or a
// type-table index there, never an inline composite. Any other kind fails
// loud rather than silently misrepresenting the type.
func specToTypeRef(raw json.RawMessage, nameToIndex map[string]uint32) (binary.TypeRef, error) {
	var node typeSpecNode
	if err := json.Unmarshal(raw, &node); err != nil {
		return binary.TypeRef{}, fmt.Errorf("decoding type spec: %w", err)
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
		return binary.TypeRef{}, fmt.Errorf("composite kind %q used inline; battery must reference it via a top-level entry + \"ref\"", node.Kind)
	}
}

// buildTypeDesc converts a top-level (or ref-resolved) TypeSpec JSON node
// into a binary.TypeDesc.
func buildTypeDesc(raw json.RawMessage, nameToIndex map[string]uint32) (binary.TypeDesc, error) {
	var node typeSpecNode
	if err := json.Unmarshal(raw, &node); err != nil {
		return nil, fmt.Errorf("decoding type spec: %w", err)
	}

	switch node.Kind {
	case "primitive":
		return binary.PrimitiveDesc{Prim: node.Prim}, nil

	case "record":
		fields := make([]binary.RecordField, len(node.Fields))
		for i, f := range node.Fields {
			tr, err := specToTypeRef(f.Type, nameToIndex)
			if err != nil {
				return nil, fmt.Errorf("record field %q: %w", f.Name, err)
			}
			fields[i] = binary.RecordField{Name: f.Name, Type: tr}
		}
		return binary.RecordDesc{Fields: fields}, nil

	case "variant":
		var cases []caseSpecNode
		if err := json.Unmarshal(node.Cases, &cases); err != nil {
			return nil, fmt.Errorf("decoding variant cases: %w", err)
		}
		out := make([]binary.VariantCase, len(cases))
		for i, c := range cases {
			var tp *binary.TypeRef
			if !isJSONNull(c.Type) {
				tr, err := specToTypeRef(c.Type, nameToIndex)
				if err != nil {
					return nil, fmt.Errorf("variant case %q: %w", c.Name, err)
				}
				tp = &tr
			}
			out[i] = binary.VariantCase{Name: c.Name, Type: tp}
		}
		return binary.VariantDesc{Cases: out}, nil

	case "list":
		tr, err := specToTypeRef(node.Elem, nameToIndex)
		if err != nil {
			return nil, fmt.Errorf("list element: %w", err)
		}
		return binary.ListDesc{Element: tr}, nil

	case "tuple":
		elems := make([]binary.TypeRef, len(node.Elems))
		for i, e := range node.Elems {
			tr, err := specToTypeRef(e, nameToIndex)
			if err != nil {
				return nil, fmt.Errorf("tuple element %d: %w", i, err)
			}
			elems[i] = tr
		}
		return binary.TupleDesc{Elements: elems}, nil

	case "flags":
		return binary.FlagsDesc{Names: node.Names}, nil

	case "enum":
		var cases []string
		if err := json.Unmarshal(node.Cases, &cases); err != nil {
			return nil, fmt.Errorf("decoding enum cases: %w", err)
		}
		return binary.EnumDesc{Cases: cases}, nil

	case "option":
		tr, err := specToTypeRef(node.Elem, nameToIndex)
		if err != nil {
			return nil, fmt.Errorf("option element: %w", err)
		}
		return binary.OptionDesc{Element: tr}, nil

	case "result":
		var okRef, errRef *binary.TypeRef
		if !isJSONNull(node.Ok) {
			tr, err := specToTypeRef(node.Ok, nameToIndex)
			if err != nil {
				return nil, fmt.Errorf("result ok: %w", err)
			}
			okRef = &tr
		}
		if !isJSONNull(node.Err) {
			tr, err := specToTypeRef(node.Err, nameToIndex)
			if err != nil {
				return nil, fmt.Errorf("result err: %w", err)
			}
			errRef = &tr
		}
		return binary.ResultDesc{Ok: okRef, Err: errRef}, nil

	case "own":
		return binary.OwnDesc{ResourceType: 0}, nil

	case "borrow":
		return binary.BorrowDesc{ResourceType: 0}, nil

	default:
		return nil, fmt.Errorf("unsupported top-level kind %q", node.Kind)
	}
}

// buildFuncDesc converts a "func" TypeSpec JSON node into a binary.FuncDesc.
func buildFuncDesc(raw json.RawMessage, nameToIndex map[string]uint32) (binary.FuncDesc, error) {
	var node typeSpecNode
	if err := json.Unmarshal(raw, &node); err != nil {
		return binary.FuncDesc{}, fmt.Errorf("decoding func spec: %w", err)
	}

	params := make([]binary.FuncParam, len(node.Params))
	for i, p := range node.Params {
		tr, err := specToTypeRef(p.Type, nameToIndex)
		if err != nil {
			return binary.FuncDesc{}, fmt.Errorf("func param %q: %w", p.Name, err)
		}
		params[i] = binary.FuncParam{Name: p.Name, Type: tr}
	}

	var results binary.FuncResults
	if !isJSONNull(node.Results.Unnamed) {
		tr, err := specToTypeRef(node.Results.Unnamed, nameToIndex)
		if err != nil {
			return binary.FuncDesc{}, fmt.Errorf("func unnamed result: %w", err)
		}
		results.Unnamed = &tr
	} else {
		named := make([]binary.FuncResult, len(node.Results.Named))
		for i, r := range node.Results.Named {
			tr, err := specToTypeRef(r.Type, nameToIndex)
			if err != nil {
				return binary.FuncDesc{}, fmt.Errorf("func named result %q: %w", r.Name, err)
			}
			named[i] = binary.FuncResult{Name: r.Name, Type: tr}
		}
		results.Named = named
	}

	return binary.FuncDesc{Params: params, Results: results}, nil
}

// ------- The oracle test itself -------

func TestOracleAgainstReference(t *testing.T) {
	typesData, err := os.ReadFile("testdata/oracle_types.json")
	if err != nil {
		t.Fatalf("reading oracle_types.json: %v", err)
	}
	var typesFile oracleTypesFile
	if err := json.Unmarshal(typesData, &typesFile); err != nil {
		t.Fatalf("parsing oracle_types.json: %v", err)
	}
	if len(typesFile.Types) == 0 {
		t.Fatal("oracle_types.json battery is empty")
	}

	goldenData, err := os.ReadFile("testdata/oracle_golden.json")
	if err != nil {
		t.Fatalf("reading oracle_golden.json: %v", err)
	}
	var golden map[string]goldenEntry
	if err := json.Unmarshal(goldenData, &golden); err != nil {
		t.Fatalf("parsing oracle_golden.json: %v", err)
	}

	// Assign each battery entry a stable index (its position) so "ref" nodes
	// can be turned into TypeRef{TypeIndex}. The battery only contains
	// backward references, so a single top-to-bottom pass that both builds
	// and resolves suffices.
	nameToIndex := make(map[string]uint32, len(typesFile.Types))
	for i, entry := range typesFile.Types {
		nameToIndex[entry.Name] = uint32(i)
	}

	resolveMap := make(map[uint32]binary.TypeDesc, len(typesFile.Types))
	resolve := func(idx uint32) binary.TypeDesc {
		desc, ok := resolveMap[idx]
		if !ok {
			panic(fmt.Sprintf("oracle: unresolved type index %d", idx))
		}
		return desc
	}

	seenNames := make(map[string]bool, len(typesFile.Types))

	for i, entry := range typesFile.Types {
		idx := uint32(i)

		if seenNames[entry.Name] {
			t.Fatalf("duplicate battery entry name %q", entry.Name)
		}
		seenNames[entry.Name] = true

		want, ok := golden[entry.Name]
		if !ok {
			t.Errorf("%s: no golden entry (did you run gen_oracle.py?)", entry.Name)
			continue
		}

		var kindNode struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(entry.Type, &kindNode); err != nil {
			t.Fatalf("%s: decoding kind: %v", entry.Name, err)
		}

		if kindNode.Kind == "func" {
			if want.Kind != "func" {
				t.Errorf("%s: golden kind %q, battery kind \"func\"", entry.Name, want.Kind)
				continue
			}
			fd, err := buildFuncDesc(entry.Type, nameToIndex)
			if err != nil {
				t.Fatalf("%s: building func desc: %v", entry.Name, err)
			}

			t.Run(entry.Name+"/lift", func(t *testing.T) {
				params, results, err := FlattenFunc(fd, resolve, "lift")
				if err != nil {
					t.Fatalf("FlattenFunc(lift): %v", err)
				}
				assertStringSlice(t, "params", params, want.Lift.Params)
				assertStringSlice(t, "results", results, want.Lift.Results)
			})
			t.Run(entry.Name+"/lower", func(t *testing.T) {
				params, results, err := FlattenFunc(fd, resolve, "lower")
				if err != nil {
					t.Fatalf("FlattenFunc(lower): %v", err)
				}
				assertStringSlice(t, "params", params, want.Lower.Params)
				assertStringSlice(t, "results", results, want.Lower.Results)
			})
			continue
		}

		if want.Kind != "value" {
			t.Errorf("%s: golden kind %q, battery kind %q", entry.Name, want.Kind, kindNode.Kind)
			continue
		}

		desc, err := buildTypeDesc(entry.Type, nameToIndex)
		if err != nil {
			t.Fatalf("%s: building type desc: %v", entry.Name, err)
		}
		resolveMap[idx] = desc

		t.Run(entry.Name, func(t *testing.T) {
			size, err := Size(desc, resolve)
			if err != nil {
				t.Fatalf("Size: %v", err)
			}
			if size != want.Size {
				t.Errorf("Size = %d, want %d (reference definitions.py)", size, want.Size)
			}

			align, err := Alignment(desc, resolve)
			if err != nil {
				t.Fatalf("Alignment: %v", err)
			}
			if align != want.Alignment {
				t.Errorf("Alignment = %d, want %d (reference definitions.py)", align, want.Alignment)
			}

			flat, err := Flatten(desc, resolve)
			if err != nil {
				t.Fatalf("Flatten: %v", err)
			}
			assertStringSlice(t, "Flatten", flat, want.Flatten)
		})
	}
}

func assertStringSlice(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s = %v, want %v", label, got, want)
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s = %v, want %v", label, got, want)
			return
		}
	}
}
