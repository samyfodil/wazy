package binary

import (
	"fmt"
	"unsafe"

	"github.com/samyfodil/wazy/internal/leb128"
)

// This file implements leaf byte-walking helpers for the component-model
// type grammar (deftype / defvaltype / functype / instancetype /
// componenttype / resourcetype); typedecl.go walks the declarations of an
// instance or component type.
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
//
// The name is a view of buf, not a copy: every buffer the decoder reads is
// its own copy of the binary (see Decode and DecodeBytes), which nothing
// writes after, and which the decoded Component keeps as Bytes anyway. A
// copy per name was most of what decoding a component allocated.
func readLabel(buf []byte, off int) (string, int, error) {
	length, n, err := leb128.LoadUint32(buf[off:])
	if err != nil {
		return "", off, fmt.Errorf("label length: %w", err)
	}
	off += int(n)
	if off+int(length) > len(buf) {
		return "", off, ErrTruncatedBinary
	}
	if length == 0 {
		return "", off, nil
	}
	return unsafe.String(&buf[off], int(length)), off + int(length), nil
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

// ExternDesc is one externdesc of the binary format: what an import, an
// export's type ascription, or an import/export declaration inside a
// component or instance type says the item is.
//
// Sort is the sort byte: 0x00 core module, 0x01 func, 0x02 value, 0x03 type,
// 0x04 component, 0x05 instance. For every sort but type, Index is the type
// index of the item's declared type (a core module type for 0x00, which is a
// CORE type index). For a type, Eq reports the bound: true for `(eq Index)`,
// false for `(sub resource)`, which names no index and introduces a fresh
// abstract resource type.
type ExternDesc struct {
	Sort  byte
	Index uint32
	Eq    bool
}

// readExternDesc consumes one externdesc. A value-sort (0x02) externdesc is
// refused: values are a gated feature this decoder does not model.
func readExternDesc(buf []byte, off int) (ExternDesc, int, error) {
	if off >= len(buf) {
		return ExternDesc{}, off, ErrTruncatedBinary
	}
	d := ExternDesc{Sort: buf[off]}
	off++
	switch d.Sort {
	case 0x00: // core:type -- expect 0x11 then core typeidx
		if off >= len(buf) || buf[off] != 0x11 {
			return d, off, fmt.Errorf("externdesc: expected core module type prefix 0x11")
		}
		off++
	case 0x01, 0x04, 0x05: // func, component, instance: typeidx
	case 0x03: // type bound: 0x00 typeidx (eq) | 0x01 (sub resource)
		if off >= len(buf) {
			return d, off, ErrTruncatedBinary
		}
		bound := buf[off]
		off++
		switch bound {
		case 0x00:
			d.Eq = true
		case 0x01:
			return d, off, nil
		default:
			return d, off, fmt.Errorf("externdesc: invalid type bound %#x", bound)
		}
	default:
		return d, off, fmt.Errorf("unsupported (M1): externdesc sort %#x", d.Sort)
	}
	idx, n, err := leb128.LoadUint32(buf[off:])
	if err != nil {
		return d, off, err
	}
	d.Index = idx
	return d, off + int(n), nil
}

// readComponenttypeDesc reads vec(componentdecl) (the 0x41 tag already
// read).
func readComponenttypeDesc(buf []byte, off int) (ComponentDesc, int, error) {
	b, off, err := readTypeBody(buf, off, true)
	if err != nil {
		return ComponentDesc{}, off, err
	}
	return ComponentDesc{Types: b.types, Outer: b.outer, Decls: b.decls}, off, nil
}
