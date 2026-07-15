package binary

import (
	"fmt"

	"github.com/samyfodil/wazy/internal/leb128"
)

// This file implements a structural decoder for the component-model type
// grammar (deftype / defvaltype / functype / instancetype / componenttype /
// resourcetype). It only needs to consume each construct's exact bytes so the
// type/import/export section walkers stay in sync; it records a human-readable
// kind but does not build a semantic type graph (that is the ABI milestone).
//
// Design rule for M1: any construct we do not yet handle returns an explicit
// "unsupported (M1)" error rather than guessing a length. A loud failure that
// names the missing construct is correct; a silent mis-walk that corrupts every
// following section is the failure mode we refuse.
//
// Grammar reference: WebAssembly/component-model design/mvp/Binary.md.

// primvaltype opcodes occupy 0x73..0x7f (encoded as negative s33 values).
func isPrimValtype(b byte) bool { return b >= 0x73 && b <= 0x7f }

func primName(b byte) string {
	switch b {
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

// readValtype consumes a valtype: a single s33. Negative encodes a primitive
// (byte 0x73..0x7f); non-negative is a type index.
func readValtype(buf []byte, off int) (int, error) {
	v, n, err := leb128.LoadInt33AsInt64(buf[off:])
	if err != nil {
		return off, fmt.Errorf("valtype: %w", err)
	}
	off += int(n)
	if v < 0 {
		b := byte(v & 0x7f)
		if !isPrimValtype(b) {
			return off, fmt.Errorf("valtype: invalid primitive code %#x", b)
		}
	}
	return off, nil
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

// readOptValtype consumes an optional valtype: 0x00 (none) or 0x01 valtype.
func readOptValtype(buf []byte, off int) (int, error) {
	if off >= len(buf) {
		return off, ErrTruncatedBinary
	}
	tag := buf[off]
	off++
	switch tag {
	case 0x00:
		return off, nil
	case 0x01:
		return readValtype(buf, off)
	default:
		return off, fmt.Errorf("optional valtype: invalid tag %#x", tag)
	}
}

// readLabelValtypeVec consumes vec(label valtype), used for record fields and
// named parameter/result lists.
func readLabelValtypeVec(buf []byte, off int) (int, error) {
	count, n, err := leb128.LoadUint32(buf[off:])
	if err != nil {
		return off, err
	}
	off += int(n)
	for i := uint32(0); i < count; i++ {
		if _, off, err = readLabel(buf, off); err != nil {
			return off, err
		}
		if off, err = readValtype(buf, off); err != nil {
			return off, err
		}
	}
	return off, nil
}

// readLabelVec consumes vec(label), used for flags and enum cases.
func readLabelVec(buf []byte, off int) (int, error) {
	count, n, err := leb128.LoadUint32(buf[off:])
	if err != nil {
		return off, err
	}
	off += int(n)
	for i := uint32(0); i < count; i++ {
		if _, off, err = readLabel(buf, off); err != nil {
			return off, err
		}
	}
	return off, nil
}

// readResultList consumes a functype result list: 0x00 valtype (single
// unnamed) or 0x01 vec(label valtype) (named).
func readResultList(buf []byte, off int) (int, error) {
	if off >= len(buf) {
		return off, ErrTruncatedBinary
	}
	tag := buf[off]
	off++
	switch tag {
	case 0x00:
		return readValtype(buf, off)
	case 0x01:
		return readLabelValtypeVec(buf, off)
	default:
		return off, fmt.Errorf("result list: invalid tag %#x", tag)
	}
}

// readFunctype consumes a functype body (the 0x40 tag is already read):
// paramlist = vec(label valtype), then a result list.
func readFunctype(buf []byte, off int) (int, error) {
	off, err := readLabelValtypeVec(buf, off)
	if err != nil {
		return off, fmt.Errorf("functype params: %w", err)
	}
	off, err = readResultList(buf, off)
	if err != nil {
		return off, fmt.Errorf("functype results: %w", err)
	}
	return off, nil
}

// readDefvaltype consumes a defvaltype body given its already-read tag.
func readDefvaltype(buf []byte, off int, tag byte) (int, error) {
	if isPrimValtype(tag) {
		return off, nil
	}
	var err error
	switch tag {
	case 0x72: // record
		off, err = readLabelValtypeVec(buf, off)
	case 0x71: // variant
		off, err = readVariant(buf, off)
	case 0x70, 0x6b: // list (0x70) or option (0x6b): one valtype
		off, err = readValtype(buf, off)
	case 0x6f: // tuple: vec(valtype)
		off, err = readValtypeVec(buf, off)
	case 0x6e, 0x6d: // flags, enum: vec(label)
		off, err = readLabelVec(buf, off)
	case 0x6a: // result: opt(valtype) opt(valtype)
		if off, err = readOptValtype(buf, off); err == nil {
			off, err = readOptValtype(buf, off)
		}
	case 0x69, 0x68: // own, borrow: typeidx
		_, n, e := leb128.LoadUint32(buf[off:])
		if e != nil {
			return off, e
		}
		off += int(n)
	default:
		return off, fmt.Errorf("unsupported (M1): defvaltype tag %#x", tag)
	}
	return off, err
}

func readValtypeVec(buf []byte, off int) (int, error) {
	count, n, err := leb128.LoadUint32(buf[off:])
	if err != nil {
		return off, err
	}
	off += int(n)
	for i := uint32(0); i < count; i++ {
		if off, err = readValtype(buf, off); err != nil {
			return off, err
		}
	}
	return off, nil
}

// readVariant consumes vec(case) where
//
//	case ::= label option(valtype) option(refines:u32)
//
// The refines index is retained in the binary grammar as an option but is
// always absent (0x00) in current output.
func readVariant(buf []byte, off int) (int, error) {
	count, n, err := leb128.LoadUint32(buf[off:])
	if err != nil {
		return off, err
	}
	off += int(n)
	for i := uint32(0); i < count; i++ {
		if _, off, err = readLabel(buf, off); err != nil {
			return off, err
		}
		if off, err = readOptValtype(buf, off); err != nil {
			return off, err
		}
		// option(refines:u32)
		if off >= len(buf) {
			return off, ErrTruncatedBinary
		}
		switch buf[off] {
		case 0x00:
			off++
		case 0x01:
			off++
			_, m, e := leb128.LoadUint32(buf[off:])
			if e != nil {
				return off, e
			}
			off += int(m)
		default:
			return off, fmt.Errorf("variant case refines: invalid tag %#x", buf[off])
		}
	}
	return off, nil
}

// readExterndesc consumes an externdesc (used by import/export/instance/
// component decls) and returns the sort byte. Core module uses the two-byte
// prefix 0x00 0x11.
func readExterndesc(buf []byte, off int) (sort byte, _ int, err error) {
	if off >= len(buf) {
		return 0, off, ErrTruncatedBinary
	}
	sort = buf[off]
	off++
	switch sort {
	case 0x00: // core:type — expect 0x11 then core typeidx
		if off >= len(buf) || buf[off] != 0x11 {
			return sort, off, fmt.Errorf("externdesc: expected core module type prefix 0x11")
		}
		off++
		_, n, e := leb128.LoadUint32(buf[off:])
		if e != nil {
			return sort, off, e
		}
		off += int(n)
	case 0x01, 0x04, 0x05: // func, component, instance: typeidx
		_, n, e := leb128.LoadUint32(buf[off:])
		if e != nil {
			return sort, off, e
		}
		off += int(n)
	case 0x03: // type bound: 0x00 typeidx (eq) | 0x01 (sub)
		if off >= len(buf) {
			return sort, off, ErrTruncatedBinary
		}
		bound := buf[off]
		off++
		if bound == 0x00 {
			_, n, e := leb128.LoadUint32(buf[off:])
			if e != nil {
				return sort, off, e
			}
			off += int(n)
		}
	default:
		return sort, off, fmt.Errorf("unsupported (M1): externdesc sort %#x", sort)
	}
	return sort, off, nil
}

// readInstanceDecl consumes one instancedecl.
func readInstanceDecl(buf []byte, off int) (int, error) {
	if off >= len(buf) {
		return off, ErrTruncatedBinary
	}
	tag := buf[off]
	off++
	switch tag {
	case 0x01: // type: a deftype
		_, off2, err := readDeftype(buf, off)
		return off2, err
	case 0x04: // export decl: externname externdesc
		_, off2, err := readExternName(buf, off)
		if err != nil {
			return off2, err
		}
		_, off2, err = readExterndesc(buf, off2)
		return off2, err
	case 0x02: // alias
		return readAlias(buf, off)
	case 0x00: // core:type
		return off, fmt.Errorf("unsupported (M1): core type in instance type")
	default:
		return off, fmt.Errorf("instancedecl: invalid tag %#x", tag)
	}
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

// readInstancetype consumes vec(instancedecl) (the 0x42 tag already read).
func readInstancetype(buf []byte, off int) (int, error) {
	count, n, err := leb128.LoadUint32(buf[off:])
	if err != nil {
		return off, err
	}
	off += int(n)
	for i := uint32(0); i < count; i++ {
		if off, err = readInstanceDecl(buf, off); err != nil {
			return off, fmt.Errorf("instancedecl[%d]: %w", i, err)
		}
	}
	return off, nil
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
		_, off2, err = readExterndesc(buf, off2)
		return off2, err
	}
	return readInstanceDecl(buf, off)
}

// readComponenttype consumes vec(componentdecl) (the 0x41 tag already read).
func readComponenttype(buf []byte, off int) (int, error) {
	count, n, err := leb128.LoadUint32(buf[off:])
	if err != nil {
		return off, err
	}
	off += int(n)
	for i := uint32(0); i < count; i++ {
		if off, err = readComponentDecl(buf, off); err != nil {
			return off, fmt.Errorf("componentdecl[%d]: %w", i, err)
		}
	}
	return off, nil
}

// readResourcetype consumes a resourcetype body (0x3f tag already read):
// a rep valtype byte then a destructor option (0x00 none | 0x01 funcidx).
func readResourcetype(buf []byte, off int) (int, error) {
	if off, err := readValtype(buf, off); err != nil {
		return off, err
	} else if off >= len(buf) {
		return off, ErrTruncatedBinary
	} else {
		dtor := buf[off]
		off++
		if dtor == 0x01 {
			_, n, e := leb128.LoadUint32(buf[off:])
			if e != nil {
				return off, e
			}
			off += int(n)
		} else if dtor != 0x00 {
			return off, fmt.Errorf("resourcetype: invalid destructor tag %#x", dtor)
		}
		return off, nil
	}
}

// readDeftype consumes one deftype and returns a human-readable kind.
func readDeftype(buf []byte, off int) (kind string, _ int, err error) {
	if off >= len(buf) {
		return "", off, ErrTruncatedBinary
	}
	tag := buf[off]
	off++
	switch {
	case tag == 0x40:
		off, err = readFunctype(buf, off)
		return "func", off, err
	case tag == 0x41:
		off, err = readComponenttype(buf, off)
		return "component", off, err
	case tag == 0x42:
		off, err = readInstancetype(buf, off)
		return "instance", off, err
	case tag == 0x3f:
		off, err = readResourcetype(buf, off)
		return "resource", off, err
	case isPrimValtype(tag):
		return primName(tag), off, nil
	default:
		off, err = readDefvaltype(buf, off, tag)
		return defvaltypeName(tag), off, err
	}
}

func defvaltypeName(tag byte) string {
	switch tag {
	case 0x72:
		return "record"
	case 0x71:
		return "variant"
	case 0x70:
		return "list"
	case 0x6f:
		return "tuple"
	case 0x6e:
		return "flags"
	case 0x6d:
		return "enum"
	case 0x6b:
		return "option"
	case 0x6a:
		return "result"
	case 0x69:
		return "own"
	case 0x68:
		return "borrow"
	default:
		return fmt.Sprintf("type(%#x)", tag)
	}
}
