package instance

import (
	"bytes"
	"fmt"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/leb128"
)

// This file hand-encodes minimal core WebAssembly binaries for "passthrough
// shim" modules: a module whose only job is to import a func/memory/table
// from an already-instantiated real module (by its wazy-registered name) and
// immediately re-export it, verbatim, under a (possibly different) name.
//
// # Why this exists
//
// The general Component Model instantiation graph (see instantiateWithImports
// in host_import.go) regroups already-defined core items under new names via
// "inline-export" core instances (core:instance kind 0x01) -- e.g. a real
// guest module's own "memory" export gets re-grouped, alone, into a synthetic
// instance that a *different* core module then imports as "env"."memory".
//
// wazy's public API has no way to build a *host* module (r.NewHostModuleBuilder)
// that exports a memory or table: HostModuleBuilder only supports Go-backed
// funcs (see builder.go), and api.Module has no Table() accessor at all (see
// the "TODO: Table" note on api.Module). But per the Component Model's
// "shared-everything" semantics, a re-grouped memory or table MUST be the
// exact same underlying object as its source -- e.g. module1's adapter code
// reads/writes offsets that module0's _start already computed, through
// module1's own load/store instructions against an *imported* memory, so a
// copy would silently desync the two modules' views of memory.
//
// A real (non-host) core module that imports an item and exports it right
// back, unmodified, achieves this for free: wazy's own module-linking
// resolves such an import by sharing the underlying MemoryInstance/
// TableInstance object (see internal/wasm/store.go's resolveImports), exactly
// like any two real wasm modules linked together. So rather than extend
// wazy's public surface (a much larger change), this package hand-encodes the
// (tiny, purely mechanical -- no instructions, no code section at all, since
// every item is imported rather than locally defined) wasm bytes for exactly
// that shape and feeds them through the existing public Runtime.InstantiateWithConfig.
//
// Func items could alternatively be forwarded via a Go host-module trampoline
// (calling the source's ExportedFunction), which is behaviorally equivalent
// for funcs (a func has no mutable identity the way memory/table do). This
// package always uses the shim encoding for uniformity, so a single
// mixed-sort inline-export group (e.g. real_hello's core instance 15, which
// groups both lowered-import funcs and a shared function table) becomes one
// module rather than being artificially split.

// shimSort is the core:sort of one passthrough item, matching the
// core-wasm-binary importdesc/exportdesc discriminator (funcs 0x00, tables
// 0x01, memories 0x02).
type shimSort byte

const (
	shimSortFunc   shimSort = 0x00
	shimSortTable  shimSort = 0x01
	shimSortMemory shimSort = 0x02
)

// shimItem is one passthrough entry: import (fromModule, fromName) and
// re-export it as exportName. Params/Results are only meaningful (and
// required) when Sort == shimSortFunc; tables are always declared funcref
// with no bounds (min 0, no max -- always satisfiable against any real
// table, see buildPassthroughShim), and memories are always declared with no
// bounds either, for the same reason.
type shimItem struct {
	Sort       shimSort
	FromModule string
	FromName   string
	ExportName string
	Params     []api.ValueType // Sort == shimSortFunc only
	Results    []api.ValueType // Sort == shimSortFunc only
}

// buildPassthroughShim encodes a minimal core wasm binary that imports every
// item in items (by its FromModule/FromName) and re-exports it, unmodified,
// under ExportName. It has no function bodies at all -- every func is
// imported, never locally defined -- so no code section is emitted.
//
// Table items are declared with element type funcref and memory items with
// no explicit bounds; a wasm import validates successfully as long as the
// *declared* bounds are less than or equal to the real exporter's (an
// always-true statement for min=0/no-max), so this is safe regardless of the
// real item's actual size -- see the resolveImports bounds checks in
// internal/wasm/store.go, which this deliberately relies on without needing
// to duplicate their logic here.
func buildPassthroughShim(items []shimItem) ([]byte, error) {
	if len(items) == 0 {
		return nil, fmt.Errorf("component/instance: buildPassthroughShim: no items")
	}

	var typeSec, importSec, exportSec bytes.Buffer
	var typeCount, importCount, exportCount uint32
	var funcIdx, tableIdx, memIdx uint32

	for i, it := range items {
		if it.FromModule == "" || it.FromName == "" {
			return nil, fmt.Errorf("component/instance: buildPassthroughShim: item[%d] has an empty source module/name", i)
		}
		if it.ExportName == "" {
			return nil, fmt.Errorf("component/instance: buildPassthroughShim: item[%d] has an empty export name", i)
		}

		switch it.Sort {
		case shimSortFunc:
			typeIdx := typeCount
			writeFuncType(&typeSec, it.Params, it.Results)
			typeCount++

			writeName(&importSec, it.FromModule)
			writeName(&importSec, it.FromName)
			importSec.WriteByte(0x00) // importdesc: func
			importSec.Write(leb128.EncodeUint32(typeIdx))
			importCount++

			writeName(&exportSec, it.ExportName)
			exportSec.WriteByte(0x00) // exportdesc: func
			exportSec.Write(leb128.EncodeUint32(funcIdx))
			exportCount++
			funcIdx++

		case shimSortTable:
			writeName(&importSec, it.FromModule)
			writeName(&importSec, it.FromName)
			importSec.WriteByte(0x01) // importdesc: table
			importSec.WriteByte(0x70) // elemtype: funcref
			writeLimits(&importSec, 0)
			importCount++

			writeName(&exportSec, it.ExportName)
			exportSec.WriteByte(0x01) // exportdesc: table
			exportSec.Write(leb128.EncodeUint32(tableIdx))
			exportCount++
			tableIdx++

		case shimSortMemory:
			writeName(&importSec, it.FromModule)
			writeName(&importSec, it.FromName)
			importSec.WriteByte(0x02) // importdesc: memory
			writeLimits(&importSec, 0)
			importCount++

			writeName(&exportSec, it.ExportName)
			exportSec.WriteByte(0x02) // exportdesc: memory
			exportSec.Write(leb128.EncodeUint32(memIdx))
			exportCount++
			memIdx++

		default:
			return nil, fmt.Errorf("component/instance: buildPassthroughShim: item[%d] has unsupported sort %#x", i, it.Sort)
		}
	}

	var out bytes.Buffer
	out.Write([]byte{0x00, 0x61, 0x73, 0x6d}) // magic
	out.Write([]byte{0x01, 0x00, 0x00, 0x00}) // version 1

	if typeCount > 0 {
		writeSection(&out, 1, prefixCount(typeCount, typeSec.Bytes()))
	}
	writeSection(&out, 2, prefixCount(importCount, importSec.Bytes()))
	writeSection(&out, 7, prefixCount(exportCount, exportSec.Bytes()))

	return out.Bytes(), nil
}

// writeFuncType appends a functype (0x60 vec(valtype) vec(valtype)) to buf.
func writeFuncType(buf *bytes.Buffer, params, results []api.ValueType) {
	buf.WriteByte(0x60)
	buf.Write(leb128.EncodeUint32(uint32(len(params))))
	buf.Write(params)
	buf.Write(leb128.EncodeUint32(uint32(len(results))))
	buf.Write(results)
}

// writeLimits appends a no-max limits (0x00 min) to buf -- the only shape
// buildPassthroughShim ever needs (see its doc: a declared bound of min=0/no
// max always validates against any real table or memory, regardless of its
// actual size).
func writeLimits(buf *bytes.Buffer, min uint32) {
	buf.WriteByte(0x00)
	buf.Write(leb128.EncodeUint32(min))
}

// writeName appends a wasm name (vec(byte): LEB128 length + raw utf8) to buf.
func writeName(buf *bytes.Buffer, name string) {
	buf.Write(leb128.EncodeUint32(uint32(len(name))))
	buf.WriteString(name)
}

// prefixCount prepends a LEB128-encoded element count to a section's already-
// encoded body, as every core wasm vec-shaped section requires.
func prefixCount(count uint32, body []byte) []byte {
	var out bytes.Buffer
	out.Write(leb128.EncodeUint32(count))
	out.Write(body)
	return out.Bytes()
}

// writeSection appends a full section (id + LEB128 size + body) to out.
func writeSection(out *bytes.Buffer, id byte, body []byte) {
	out.WriteByte(id)
	out.Write(leb128.EncodeUint32(uint32(len(body))))
	out.Write(body)
}
