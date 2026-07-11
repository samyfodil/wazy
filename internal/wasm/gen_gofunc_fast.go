//go:build ignore

// Command gen_gofunc_fast regenerates gofunc_fast.go, the table of
// zero-reflection adapters used to call host functions whose concrete Go
// signature is one of the common shapes registered via
// HostFunctionBuilder.WithFunc.
//
// The adapters are deliberately repetitive: there is one small struct plus a
// hand-shaped Call method for every (param-kind x param-types x result-type)
// combination in the matrix below. That repetition is essential complexity,
// not an oversight:
//
//   - The dispatch must be reflection-free at call time, so each Call has to
//     name its concrete func type in a type assertion. That cannot be shared
//     across signatures.
//   - The adapter stores the function as a reflect.Value rather than a typed
//     func field, so that Code.GoFunc keeps supporting reflect.DeepEqual-based
//     equality (a non-nil typed func field makes DeepEqual always report the
//     enclosing struct as unequal; a reflect.Value wrapping the same func does
//     not). See gofunc.go and host_test.go.
//
// Because the shape is fixed but voluminous, we generate it instead of
// hand-maintaining ~400 near-identical blocks. To change the covered matrix,
// edit the paramShapes/resultTypes tables here and run:
//
//	go generate ./internal/wasm/...
//
// This program is stdlib-only and needs no testdata.
package main

import (
	"bytes"
	"fmt"
	"go/format"
	"os"
	"strings"
)

// atom is a single wasm value slot bound to a concrete Go type. It is the unit
// from which both parameter lists and (optional) results are built.
type atom struct {
	name   string // identifier fragment used in generated type names, e.g. "u32"
	goType string // concrete Go type, e.g. "uint32"
}

var (
	u32 = atom{"u32", "uint32"}
	i32 = atom{"i32", "int32"}
	u64 = atom{"u64", "uint64"}
	i64 = atom{"i64", "int64"}
	f32 = atom{"f32", "float32"}
	f64 = atom{"f64", "float64"}
)

// decode returns the Go expression that reads slot expression src (an untyped
// uint64 stack slot) as this atom's Go type. It mirrors the parameter decoding
// performed by callGoFunc in gofunc.go so the fast and reflect paths agree bit
// for bit, including sign extension and float bit patterns.
func (a atom) decode(src string) string {
	switch a {
	case u32:
		return "uint32(" + src + ")"
	case i32:
		return "int32(" + src + ")"
	case u64:
		return src
	case i64:
		return "int64(" + src + ")"
	case f32:
		// The extra float64 round-trip mirrors callGoFunc, which decodes f32
		// params via reflect.Value.SetFloat (a float64). It is bit-identical
		// for finite values and matches the reflect path's NaN-payload
		// quieting for signaling NaNs, so the two paths agree bit for bit.
		return "float32(float64(math.Float32frombits(uint32(" + src + "))))"
	case f64:
		return "math.Float64frombits(" + src + ")"
	}
	panic("unreachable")
}

// encode returns the statement storing result expression expr (of this atom's
// Go type) back into stack[0]. It mirrors the result encoding performed by
// callGoFunc in gofunc.go.
func (a atom) encode(expr string) string {
	switch a {
	case u32:
		return "stack[0] = uint64(" + expr + ")"
	case i32:
		return "stack[0] = uint64(int64(" + expr + "))"
	case u64:
		return "stack[0] = " + expr
	case i64:
		return "stack[0] = uint64(" + expr + ")"
	case f32:
		// See the f32 note in decode: the float64 round-trip matches
		// callGoFunc, which encodes f32 results via reflect.Value.Float
		// (a float64), so signaling-NaN payloads quiet identically.
		return "stack[0] = uint64(math.Float32bits(float32(float64(" + expr + "))))"
	case f64:
		return "stack[0] = math.Float64bits(" + expr + ")"
	}
	panic("unreachable")
}

// group is one of the three ways a host function may receive the leading
// context.Context / api.Module parameters. Each maps to a distinct adapter
// interface and Call signature.
type group struct {
	name      string   // identifier fragment, e.g. "ctxmod"
	iface     string   // api interface the adapter implements
	callPar   string   // Call method parameter list
	leadTypes []string // leading func parameter types (before wasm params)
	leadArgs  []string // leading call arguments passed to fn
}

var groups = []group{
	{
		name:      "ctxmod",
		iface:     "api.GoModuleFunction",
		callPar:   "ctx context.Context, mod api.Module, stack []uint64",
		leadTypes: []string{"context.Context", "api.Module"},
		leadArgs:  []string{"ctx", "mod"},
	},
	{
		name:      "ctx",
		iface:     "api.GoFunction",
		callPar:   "ctx context.Context, stack []uint64",
		leadTypes: []string{"context.Context"},
		leadArgs:  []string{"ctx"},
	},
	{
		// The no-context adapter still satisfies api.GoFunction, so its Call
		// receives ctx, but the underlying func does not take it.
		name:      "none",
		iface:     "api.GoFunction",
		callPar:   "ctx context.Context, stack []uint64",
		leadTypes: nil,
		leadArgs:  nil,
	},
}

// paramShapes is the set of wasm parameter lists handled without reflection.
// It targets the shapes real host functions use: WASI-style i32/u32/u64
// pointer+length signatures and math-style float signatures. Shapes outside
// this table (multi-result functions and uintptr/externref params) stay on the
// reflect fallback in gofunc.go on purpose.
var paramShapes = [][]atom{
	{},                   // no params
	{u32},                // WASI/math single arg
	{i32},                // signed single arg
	{u64},                // 64-bit single arg (offsets, clocks)
	{i64},                // signed 64-bit single arg
	{f32},                // math single arg
	{f64},                // math single arg
	{u32, u32},           // WASI ptr+len, math binary
	{i32, i32},           // signed pair
	{u64, u64},           // 64-bit pair
	{f32, f32},           // math binary (f32)
	{f64, f64},           // math binary (f64)
	{u32, u64},           // mixed 32/64
	{u64, u32},           // mixed 64/32
	{i32, i64},           // mixed signed
	{u32, u32, u32},      // 3-arg WASI
	{i32, i32, i32},      // 3-arg signed
	{u32, u32, u32, u32}, // 4-arg WASI (e.g. fd_write)
}

// resultTypes is the set of single results handled without reflection, in the
// canonical order used throughout the generated file. A nil entry is the void
// (no result) case. Multi-result functions stay on the reflect fallback.
var resultTypes = []*atom{nil, &u32, &u64, &i32, &i64, &f32, &f64}

func shapeName(params []atom) string {
	if len(params) == 0 {
		return "void"
	}
	parts := make([]string, len(params))
	for i, p := range params {
		parts[i] = p.name
	}
	return strings.Join(parts, "")
}

func resultName(r *atom) string {
	if r == nil {
		return "void"
	}
	return r.name
}

// funcType renders the concrete func type asserted in a Call body, e.g.
// "func(context.Context, api.Module, uint32) float32".
func funcType(g group, params []atom, r *atom) string {
	types := append([]string{}, g.leadTypes...)
	for _, p := range params {
		types = append(types, p.goType)
	}
	sig := "func(" + strings.Join(types, ", ") + ")"
	if r != nil {
		sig += " " + r.goType
	}
	return sig
}

// callExpr renders the fn(...) call expression, decoding each wasm param from
// its stack slot.
func callExpr(g group, params []atom) string {
	args := append([]string{}, g.leadArgs...)
	for i, p := range params {
		args = append(args, p.decode(fmt.Sprintf("stack[%d]", i)))
	}
	return "fn(" + strings.Join(args, ", ") + ")"
}

func main() {
	var b bytes.Buffer

	fmt.Fprint(&b, `// Code generated by gen_gofunc_fast.go; DO NOT EDIT.

// This file provides adapters for the concrete Go host function signatures
// used most commonly by end users (see HostFunctionBuilder.WithFunc). Each
// adapter calls its underlying Go function directly with values decoded from
// the stack, avoiding the reflect.Call path in callGoFunc entirely, so
// registering a host function with one of these signatures costs zero
// allocations per call instead of the several needed by reflect.Value.Call.
//
// The function itself is kept as a reflect.Value (rather than its concrete
// type) so that Code.GoFunc keeps supporting reflect.DeepEqual-based equality:
// storing a bare typed func in a struct field makes reflect.DeepEqual always
// report the struct as unequal, even when both fields hold the same function,
// whereas a reflect.Value wrapping that func compares equal.
//
// The dispatch flow is: parseGoReflectFunc calls fastGoFunc, which selects the
// per-context switch (fastCtxmodFunc / fastCtxFunc / fastNoneFunc) by
// paramsKind, then type-switches on the concrete signature to build the
// matching adapter. A miss returns ok=false and the caller uses the general
// reflect path in gofunc.go.
//
// To change the covered matrix, edit and rerun gen_gofunc_fast.go; do not edit
// this file by hand.
package wasm

import (
	"context"
	"math"
	"reflect"

	"github.com/tetratelabs/wazero/api"
)
`)

	// Adapter types and their Call methods, grouped by context kind then by
	// the (params, result) matrix in a fixed order for deterministic output.
	for _, g := range groups {
		for _, params := range paramShapes {
			for _, r := range resultTypes {
				typeName := fmt.Sprintf("goFunc_%s_%s_%s", g.name, shapeName(params), resultName(r))
				ft := funcType(g, params, r)
				call := callExpr(g, params)

				fmt.Fprintf(&b, "\ntype %s struct{ fn reflect.Value }\n\n", typeName)
				fmt.Fprintf(&b, "func (f *%s) Call(%s) {\n", typeName, g.callPar)
				fmt.Fprintf(&b, "\tfn := f.fn.Interface().(%s)\n", ft)
				if r == nil {
					fmt.Fprintf(&b, "\t%s\n", call)
				} else {
					fmt.Fprintf(&b, "\t%s\n", r.encode(call))
				}
				fmt.Fprint(&b, "}\n")
			}
		}
	}

	// fastGoFunc dispatcher.
	fmt.Fprint(&b, `
// fastGoFunc returns a zero-allocation, reflection-free adapter for fn if its
// concrete signature is one of the common shapes in the matrix below.
// Otherwise ok is false and the caller falls back to the general reflect path.
func fastGoFunc(pk paramsKind, fn interface{}) (goFunc interface{}, ok bool) {
	switch pk {
	case paramsKindContextModule:
		return fastCtxmodFunc(fn)
	case paramsKindContext:
		return fastCtxFunc(fn)
	default:
		return fastNoneFunc(fn)
	}
}
`)

	// One type-switch function per context group.
	switchName := map[string]string{"ctxmod": "fastCtxmodFunc", "ctx": "fastCtxFunc", "none": "fastNoneFunc"}
	retType := map[string]string{"ctxmod": "api.GoModuleFunction", "ctx": "api.GoFunction", "none": "api.GoFunction"}
	for _, g := range groups {
		fmt.Fprintf(&b, "\nfunc %s(fn interface{}) (%s, bool) {\n", switchName[g.name], retType[g.name])
		fmt.Fprint(&b, "\tswitch fn := fn.(type) {\n")
		for _, params := range paramShapes {
			for _, r := range resultTypes {
				typeName := fmt.Sprintf("goFunc_%s_%s_%s", g.name, shapeName(params), resultName(r))
				ft := funcType(g, params, r)
				fmt.Fprintf(&b, "\tcase %s:\n", ft)
				fmt.Fprintf(&b, "\t\treturn &%s{fn: reflect.ValueOf(fn)}, true\n", typeName)
			}
		}
		fmt.Fprint(&b, "\tdefault:\n\t\treturn nil, false\n\t}\n}\n")
	}

	src, err := format.Source(b.Bytes())
	if err != nil {
		fmt.Fprintln(os.Stderr, b.String())
		fmt.Fprintln(os.Stderr, "gofmt failed:", err)
		os.Exit(1)
	}

	if err := os.WriteFile("gofunc_fast.go", src, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write failed:", err)
		os.Exit(1)
	}
}
