package binary

import (
	"fmt"

	"github.com/samyfodil/wazy/internal/leb128"
)

// This file implements leaf byte-walking helpers for the component-model
// type grammar (deftype / defvaltype / functype / instancetype /
// componenttype / resourcetype) plus the componenttype/componentdecl walker.
// The single structural walk that builds the semantic type graph lives in
// descriptor.go (readDeftypeDesc and friends); this file must not grow a
// second parallel walker for the same grammar.
//
// Design rule for M1: any construct we do not yet handle returns an explicit
// "unsupported (M1)" error rather than guessing a length. A loud failure that
// names the missing construct is correct; a silent mis-walk that corrupts every
// following section is the failure mode we refuse.
//
// Grammar reference: WebAssembly/component-model design/mvp/Binary.md.

// primvaltype opcodes occupy 0x73..0x7f (encoded as negative s33 values), plus
// 0x64 (error-context) added by the async ABI -- confirmed terminal/payload-free
// via `wasm-tools dump` on a func type with an error-context result: the type
// is printed as `Primitive(ErrorContext)`, the same shape as bool/string, NOT
// a defined type with a payload (contrast with future=0x65/stream=0x66, which
// DO carry an `option<valtype>` payload -- see readDefvaltypeDesc).
func isPrimValtype(b byte) bool { return (b >= 0x73 && b <= 0x7f) || b == 0x64 }

func primName(b byte) string {
	switch b {
	case 0x64:
		return "error-context"
	case 0x7f:
		return "bool"
	case 0x7e:
		return "s8"
	case 0x7d:
		return "u8"
	case 0x7c:
		return "s16"
	case 0x7b:
		return "u16"
	case 0x7a:
		return "s32"
	case 0x79:
		return "u32"
	case 0x78:
		return "s64"
	case 0x77:
		return "u64"
	case 0x76:
		return "f32"
	case 0x75:
		return "f64"
	case 0x74:
		return "char"
	case 0x73:
		return "string"
	default:
		return fmt.Sprintf("prim(%#x)", b)
	}
}

// readLabel consumes an unprefixed name (used for field/param/case/flag/enum
// labels): len:u32 followed by that many UTF-8 bytes.
func readLabel(buf []byte, off int) (string, int, error) {
	length, n, err := leb128.LoadUint32(buf[off:])
	if err != nil {
		return "", off, fmt.Errorf("label length: %w", err)
	}
	off += int(n)
	if off+int(length) > len(buf) {
		return "", off, ErrTruncatedBinary
	}
	s := string(buf[off : off+int(length)])
	return s, off + int(length), nil
}

// readExternName consumes an import/export name: a 0x00/0x01 kind byte, a
// length, and the UTF-8 bytes. 0x02 (name + attributes) is not yet handled.
func readExternName(buf []byte, off int) (string, int, error) {
	if off >= len(buf) {
		return "", off, ErrTruncatedBinary
	}
	kind := buf[off]
	off++
	switch kind {
	case 0x00, 0x01:
		return readLabel(buf, off)
	case 0x02:
		return "", off, fmt.Errorf("unsupported (M1): extern name with attributes (0x02)")
	default:
		return "", off, fmt.Errorf("extern name: invalid kind byte %#x", kind)
	}
}

// readExterndesc consumes an externdesc (used by import/export/instance/
// component decls) and returns the sort byte. Core module uses the two-byte
// prefix 0x00 0x11.
// readExterndesc reads one externdesc, returning its sort byte plus, for a
// type-sort import/export whose bound is `eq <typeidx>` (bound 0x00), that
// referenced type index in idx with hasIdx=true -- so an `import "point"
// (type (eq N))` (what wit-component/cargo-component emit for a world's
// exported types) can be resolved through to type N rather than treated as an
// opaque import. hasIdx is also true for a func-sort (0x01) externdesc, whose
// idx is the func's own type index -- decodeImportSection needs it (on
// Import.ExternIndex) to resolve a top-level func import's declared type
// (e.g. checking its Async bit against a canon lower's async option -- see
// validateAsyncOptAgreesWithType). hasIdx is false for a `sub` type bound
// (0x01, an opaque resource) and for component/instance sorts (0x04/0x05,
// whose own typeidx no caller currently needs; decode it yourself if that
// changes).
func readExterndesc(buf []byte, off int) (sort byte, idx uint32, hasIdx bool, _ int, err error) {
	if off >= len(buf) {
		return 0, 0, false, off, ErrTruncatedBinary
	}
	sort = buf[off]
	off++
	switch sort {
	case 0x00: // core:type — expect 0x11 then core typeidx
		if off >= len(buf) || buf[off] != 0x11 {
			return sort, 0, false, off, fmt.Errorf("externdesc: expected core module type prefix 0x11")
		}
		off++
		_, n, e := leb128.LoadUint32(buf[off:])
		if e != nil {
			return sort, 0, false, off, e
		}
		off += int(n)
	case 0x01: // func: typeidx
		fIdx, n, e := leb128.LoadUint32(buf[off:])
		if e != nil {
			return sort, 0, false, off, e
		}
		off += int(n)
		return sort, fIdx, true, off, nil
	case 0x04, 0x05: // component, instance: typeidx
		_, n, e := leb128.LoadUint32(buf[off:])
		if e != nil {
			return sort, 0, false, off, e
		}
		off += int(n)
	case 0x03: // type bound: 0x00 typeidx (eq) | 0x01 (sub)
		if off >= len(buf) {
			return sort, 0, false, off, ErrTruncatedBinary
		}
		bound := buf[off]
		off++
		if bound == 0x00 {
			eqIdx, n, e := leb128.LoadUint32(buf[off:])
			if e != nil {
				return sort, 0, false, off, e
			}
			off += int(n)
			return sort, eqIdx, true, off, nil
		}
	default:
		return sort, 0, false, off, fmt.Errorf("unsupported (M1): externdesc sort %#x", sort)
	}
	return sort, 0, false, off, nil
}

// readSort consumes a sort byte; the core sort (0x00) carries one extra
// discriminator byte.
func readSort(buf []byte, off int) (int, error) {
	if off >= len(buf) {
		return off, ErrTruncatedBinary
	}
	if buf[off] == 0x00 { // core sort
		if off+1 >= len(buf) {
			return off, ErrTruncatedBinary
		}
		return off + 2, nil
	}
	return off + 1, nil
}

// readAlias consumes an alias (used inside instance/component type decls):
// a sort followed by an alias target (export 0x00, core export 0x01, outer
// 0x02).
func readAlias(buf []byte, off int) (int, error) {
	off, err := readSort(buf, off)
	if err != nil {
		return off, err
	}
	if off >= len(buf) {
		return off, ErrTruncatedBinary
	}
	target := buf[off]
	off++
	readU32 := func(off int) (int, error) {
		_, n, e := leb128.LoadUint32(buf[off:])
		if e != nil {
			return off, e
		}
		return off + int(n), nil
	}
	switch target {
	case 0x00, 0x01: // (core) export: instanceidx then a name
		if off, err = readU32(off); err != nil { // instance index
			return off, err
		}
		if _, off, err = readLabel(buf, off); err != nil { // export name
			return off, err
		}
		return off, nil
	case 0x02: // outer: outer-count then index
		if off, err = readU32(off); err != nil {
			return off, err
		}
		return readU32(off)
	default:
		return off, fmt.Errorf("alias: invalid target kind %#x", target)
	}
}

// readComponentDecl consumes one componentdecl (0x03 import, else instancedecl).
func readComponentDecl(buf []byte, off int) (int, error) {
	if off >= len(buf) {
		return off, ErrTruncatedBinary
	}
	if buf[off] == 0x03 { // import decl
		off++
		_, off2, err := readExternName(buf, off)
		if err != nil {
			return off2, err
		}
		_, _, _, off2, err = readExterndesc(buf, off2)
		return off2, err
	}
	return readInstanceDeclDesc(buf, off)
}

// readComponenttype consumes vec(componentdecl) (the 0x41 tag already read).
func readComponenttype(buf []byte, off int) (int, error) {
	count, n, err := leb128.LoadUint32(buf[off:])
	if err != nil {
		return off, err
	}
	off += int(n)
	for i := range count {
		if off, err = readComponentDecl(buf, off); err != nil {
			return off, fmt.Errorf("componentdecl[%d]: %w", i, err)
		}
	}
	return off, nil
}
