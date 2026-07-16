package instance

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// TestWastConformance runs the official WebAssembly/component-model canonical-ABI
// conformance suites (test/values/*.wast) through wazy. Each .wast was split by
// `wasm-tools json-from-wast` into a spec-test manifest (module / assert_return /
// assert_trap commands) plus binary component .wasm files, vendored under
// testdata/wast/<suite>/. The manifest carries every value kind as typed JSON;
// this harness converts those to abi.Value using the export's declared param
// types, invokes the export, and compares the lifted result -- exercising both
// lowering (host->guest args) and lifting (guest->host results) against the
// reference suite, not our own oracle.
//
// concat: takes bool/all int widths/char/string/list/tuple/record/variant/enum/
//         flags/option as args, returns their concatenation -- pure ABI in+out.
// strings: string edge cases + assert_trap for out-of-bounds / invalid utf-8.
func TestWastConformance(t *testing.T) {
	// concat + strings are the canonical-ABI value suites (all value kinds in
	// and out, plus string utf-8/bounds traps). Not vendored: post-return
	// (needs the module_definition/module_instance linking model + reentrance-
	// trap builtins) and multiple-resources (nested-component composition with
	// fused resource canons) -- both large features beyond this single-
	// component runtime, not ABI bugs.
	for _, suite := range []string{"concat", "strings"} {
		t.Run(suite, func(t *testing.T) {
			runWastSuite(t, suite)
		})
	}
}

type typedVal struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
}

type wastCmd struct {
	Type     string `json:"type"`
	Filename string `json:"filename"`
	Line     int    `json:"line"`
	Action   *struct {
		Type  string     `json:"type"`
		Field string     `json:"field"`
		Args  []typedVal `json:"args"`
	} `json:"action"`
	Expected []typedVal `json:"expected"`
	Text     string     `json:"text"`
}

func runWastSuite(t *testing.T, suite string) {
	dir := filepath.Join("testdata", "wast", suite)
	raw, err := os.ReadFile(filepath.Join(dir, suite+".json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest struct {
		Commands []wastCmd `json:"commands"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}

	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	var in *Instance // the "current" component (last `module` command)
	for _, c := range manifest.Commands {
		switch c.Type {
		case "module":
			wasm, err := os.ReadFile(filepath.Join(dir, c.Filename))
			if err != nil {
				t.Fatalf("line %d: read %s: %v", c.Line, c.Filename, err)
			}
			if in != nil {
				in.Close(ctx)
			}
			in, err = Instantiate(ctx, r, wasm, WithWASI(WASIConfig{})...)
			if err != nil {
				t.Fatalf("line %d: instantiate %s: %v", c.Line, c.Filename, err)
			}

		case "assert_return", "assert_trap":
			if c.Action == nil || c.Action.Type != "invoke" {
				continue // module_definition/module_instance etc. -- not covered here
			}
			got, err := invokeWast(ctx, in, c.Action.Field, c.Action.Args)
			if c.Type == "assert_trap" {
				if err == nil {
					t.Errorf("line %d: %s: expected trap %q, got success", c.Line, c.Action.Field, c.Text)
				}
				continue
			}
			if err != nil {
				t.Errorf("line %d: %s: unexpected error: %v", c.Line, c.Action.Field, err)
				continue
			}
			want := expectedWast(t, in, c.Action.Field, c.Expected)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("line %d: %s = %#v, want %#v", c.Line, c.Action.Field, got, want)
			}
		}
	}
	if in != nil {
		in.Close(ctx)
	}
}

// invokeWast converts the typed JSON args to abi.Value using the export's
// declared param types, then calls it.
func invokeWast(ctx context.Context, in *Instance, field string, args []typedVal) ([]abi.Value, error) {
	be, ok := in.exports[field]
	if !ok {
		return nil, errNoExport(field)
	}
	vals := make([]abi.Value, len(args))
	for i, a := range args {
		v, err := in.wastConvert(be.paramTypes[i], a)
		if err != nil {
			return nil, err
		}
		vals[i] = v
	}
	return in.Call(ctx, field, vals...)
}

// expectedWast converts the typed JSON expected results to abi.Value using the
// export's declared result type, for comparison against the lifted result.
func expectedWast(t *testing.T, in *Instance, field string, exp []typedVal) []abi.Value {
	t.Helper()
	be := in.exports[field]
	out := make([]abi.Value, len(exp))
	for i, e := range exp {
		// Single-result functions carry their type in resultType; the suites
		// here never declare multiple named results.
		v, err := in.wastConvert(be.resultType, e)
		if err != nil {
			t.Fatalf("%s: convert expected[%d]: %v", field, i, err)
		}
		out[i] = v
	}
	return out
}

type noExportErr string

func (e noExportErr) Error() string { return "no such export: " + string(e) }
func errNoExport(f string) error    { return noExportErr(f) }

// wastConvert turns one typed spec-test JSON value into the abi.Value wazy's
// lower/lift use, driven by the declared component type (needed to map variant/
// enum/flags labels to indices).
func (in *Instance) wastConvert(desc binary.TypeDesc, tv typedVal) (abi.Value, error) {
	switch d := desc.(type) {
	case binary.PrimitiveDesc:
		return convertPrim(d.Prim, tv.Value)

	case binary.RecordDesc:
		var pairs [][]json.RawMessage
		if err := json.Unmarshal(tv.Value, &pairs); err != nil {
			return nil, err
		}
		out := make([]abi.Value, len(d.Fields))
		for i, f := range d.Fields {
			ft, err := resolveTypeRef(&f.Type, in.resolve)
			if err != nil {
				return nil, err
			}
			var inner typedVal
			if err := json.Unmarshal(pairs[i][1], &inner); err != nil {
				return nil, err
			}
			if out[i], err = in.wastConvert(ft, inner); err != nil {
				return nil, err
			}
		}
		return out, nil

	case binary.TupleDesc:
		elems, err := unmarshalTypedList(tv.Value)
		if err != nil {
			return nil, err
		}
		out := make([]abi.Value, len(d.Elements))
		for i := range d.Elements {
			et, err := resolveTypeRef(&d.Elements[i], in.resolve)
			if err != nil {
				return nil, err
			}
			if out[i], err = in.wastConvert(et, elems[i]); err != nil {
				return nil, err
			}
		}
		return out, nil

	case binary.ListDesc:
		elems, err := unmarshalTypedList(tv.Value)
		if err != nil {
			return nil, err
		}
		et, err := resolveTypeRef(&d.Element, in.resolve)
		if err != nil {
			return nil, err
		}
		out := make([]abi.Value, len(elems))
		for i := range elems {
			if out[i], err = in.wastConvert(et, elems[i]); err != nil {
				return nil, err
			}
		}
		return out, nil

	case binary.VariantDesc:
		var raw struct {
			Case    string    `json:"case"`
			Payload *typedVal `json:"payload"`
		}
		if err := json.Unmarshal(tv.Value, &raw); err != nil {
			return nil, err
		}
		for i, cs := range d.Cases {
			if cs.Name != raw.Case {
				continue
			}
			var payload abi.Value
			if cs.Type != nil && raw.Payload != nil {
				ct, err := resolveTypeRef(cs.Type, in.resolve)
				if err != nil {
					return nil, err
				}
				if payload, err = in.wastConvert(ct, *raw.Payload); err != nil {
					return nil, err
				}
			}
			return abi.VariantValue{Disc: uint32(i), Payload: payload}, nil
		}
		return nil, errNoExport("variant case " + raw.Case)

	case binary.EnumDesc:
		var name string
		if err := json.Unmarshal(tv.Value, &name); err != nil {
			return nil, err
		}
		for i, cs := range d.Cases {
			if cs == name {
				return uint32(i), nil
			}
		}
		return nil, errNoExport("enum case " + name)

	case binary.FlagsDesc:
		var labels []string
		if err := json.Unmarshal(tv.Value, &labels); err != nil {
			return nil, err
		}
		var bits uint32
		for _, l := range labels {
			for i, n := range d.Names {
				if n == l {
					bits |= 1 << uint(i)
				}
			}
		}
		return bits, nil

	case binary.OptionDesc:
		if string(tv.Value) == "null" {
			return nil, nil
		}
		var inner typedVal
		if err := json.Unmarshal(tv.Value, &inner); err != nil {
			return nil, err
		}
		et, err := resolveTypeRef(&d.Element, in.resolve)
		if err != nil {
			return nil, err
		}
		return in.wastConvert(et, inner)

	case binary.ResultDesc:
		var raw map[string]*typedVal
		if err := json.Unmarshal(tv.Value, &raw); err != nil {
			return nil, err
		}
		mk := func(ref *TypeRefAlias, tvp *typedVal, isErr bool) (abi.Value, error) {
			var payload abi.Value
			if ref != nil && tvp != nil {
				rt, err := resolveTypeRef((*binary.TypeRef)(ref), in.resolve)
				if err != nil {
					return nil, err
				}
				if payload, err = in.wastConvert(rt, *tvp); err != nil {
					return nil, err
				}
			}
			return abi.ResultValue{IsErr: isErr, Payload: payload}, nil
		}
		if okv, has := raw["Ok"]; has {
			return mk((*TypeRefAlias)(d.Ok), okv, false)
		}
		return mk((*TypeRefAlias)(d.Err), raw["Err"], true)
	}
	return nil, errNoExport("unsupported type " + desc.Kind())
}

// TypeRefAlias lets the result arm above cast *binary.TypeRef through a local
// name (Ok/Err are *binary.TypeRef); it is exactly binary.TypeRef.
type TypeRefAlias = binary.TypeRef

func unmarshalTypedList(raw json.RawMessage) ([]typedVal, error) {
	var out []typedVal
	err := json.Unmarshal(raw, &out)
	return out, err
}

// convertPrim maps a primitive typed JSON scalar to the Go type wazy's value
// model uses (see abi/value.go): ints arrive as decimal strings, bool as a JSON
// bool, char/string as JSON strings.
func convertPrim(prim string, raw json.RawMessage) (abi.Value, error) {
	switch prim {
	case "bool":
		var b bool
		return b, json.Unmarshal(raw, &b)
	case "u8", "u16", "u32":
		u, err := parseUintStr(raw)
		return uint32(u), err
	case "u64":
		u, err := parseUintStr(raw)
		return u, err
	case "s8", "s16", "s32":
		i, err := parseIntStr(raw)
		return int32(i), err
	case "s64":
		i, err := parseIntStr(raw)
		return i, err
	case "f32":
		// Spec-test JSON encodes floats as the decimal integer of their IEEE
		// bit pattern (like wast2json), not a decimal literal.
		u, err := parseUintStr(raw)
		return math.Float32frombits(uint32(u)), err
	case "f64":
		u, err := parseUintStr(raw)
		return math.Float64frombits(u), err
	case "char":
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		rs := []rune(s)
		if len(rs) != 1 {
			return nil, errNoExport("char value not a single rune: " + s)
		}
		return rs[0], nil
	case "string":
		var s string
		return s, json.Unmarshal(raw, &s)
	}
	return nil, errNoExport("unsupported primitive " + prim)
}

func parseUintStr(raw json.RawMessage) (uint64, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, err
	}
	return strconv.ParseUint(s, 10, 64)
}

func parseIntStr(raw json.RawMessage) (int64, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, err
	}
	return strconv.ParseInt(s, 10, 64)
}
