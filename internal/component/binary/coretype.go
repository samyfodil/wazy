package binary

import (
	"fmt"

	"github.com/samyfodil/wazy/internal/leb128"
)

// This file decodes core types as a component declares them -- the core type
// section (section 3), and a core:type inside a component or instance type --
// and the core module types among them in full: every import and export, of
// every core extern sort, and the types and aliases a module type declares.
//
// A core type the component model only carries, never interprets, is kept as
// its encoding: a core:rectype, exactly as a core module's type section
// encodes it, and a core:externtype exactly as a core import section does.
// That is the form a core WebAssembly decoder reads, which is what a type
// check comparing a core module against a module type needs: it can decode
// both with the same decoder and compare like with like.

// CoreTypeDef is one core:type definition: a core module type, or a
// core:rectype -- one or more func, struct or array types, a recursion group
// when more than one.
type CoreTypeDef struct {
	// Module is the module type, nil for a rectype.
	Module *CoreModuleType

	// Rec is a rectype's encoding as a core type section has it: a
	// component's top-level `0x00 0x50` non-final sub is re-encoded as the
	// plain `0x50` a core decoder expects.
	Rec []byte

	// Count is the number of core type indices the definition introduces:
	// the size of its recursion group, and 1 for a module type.
	Count uint32
}

// CoreModuleType is a core module type: its declarations, in order. Each
// type declaration and type alias opens the next index of the module type's
// own core type index space, which starts empty; a func or tag extern type
// names an index there.
type CoreModuleType struct {
	Decls []CoreModuleDecl
}

// CoreModuleDeclKind says what one declaration of a core module type is. Its
// values are the declaration tags of the binary format.
type CoreModuleDeclKind byte

const (
	// CoreModuleDeclImport is an import: Module, Name and Extern.
	CoreModuleDeclImport CoreModuleDeclKind = 0x00
	// CoreModuleDeclType is a type definition: Rec and RecCount.
	CoreModuleDeclType CoreModuleDeclKind = 0x01
	// CoreModuleDeclAlias is an outer alias of a core type: AliasCount and
	// AliasIndex.
	CoreModuleDeclAlias CoreModuleDeclKind = 0x02
	// CoreModuleDeclExport is an export: Name and Extern.
	CoreModuleDeclExport CoreModuleDeclKind = 0x03
)

// CoreModuleDecl is one declaration of a core module type.
type CoreModuleDecl struct {
	Kind CoreModuleDeclKind

	// Module and Name are an import's module and field names, and Name an
	// export's name.
	Module, Name string

	// Extern is the declared type of an import or export.
	Extern CoreExternType

	// Rec is a type declaration's core:rectype, and RecCount the number of
	// type indices it introduces.
	Rec      []byte
	RecCount uint32

	// AliasCount and AliasIndex are an outer type alias's target: the
	// AliasIndex-th core type of the AliasCount-th enclosing component (0:
	// the component holding the module type), never a module type.
	AliasCount, AliasIndex uint32
}

// CoreExternType is a core:externtype, as a core import section encodes it.
type CoreExternType struct {
	// Kind is the core extern sort: 0x00 func, 0x01 table, 0x02 memory,
	// 0x03 global, 0x04 tag.
	Kind byte

	// TypeIndex is a func's or tag's type, an index into the enclosing
	// module type's core type index space.
	TypeIndex uint32

	// Bytes is the encoding after the kind byte: what a core import
	// section has for this desc.
	Bytes []byte
}

// CoreTypeSpaceEntryKind says what produced an index of a component's core
// type index space.
type CoreTypeSpaceEntryKind byte

const (
	// CoreTypeFromDefinition is a core type section definition: one member
	// of CoreTypes[Def].
	CoreTypeFromDefinition CoreTypeSpaceEntryKind = iota
	// CoreTypeFromAlias is an outer alias of a core type: Aliases[Alias].
	CoreTypeFromAlias
)

// CoreTypeSpaceEntry is one index of a component's core type index space.
type CoreTypeSpaceEntry struct {
	Kind CoreTypeSpaceEntryKind
	// Def and Member name a definition's Member-th type (the position in
	// its recursion group).
	Def, Member uint32
	// Alias is the alias's index in Component.Aliases.
	Alias uint32
}

// decodeCoreTypeSection decodes a core type section: vec(core:type).
func decodeCoreTypeSection(buf []byte, offset int, sectionSize uint32) ([]CoreTypeDef, int, error) {
	end := offset + int(sectionSize)
	if end > len(buf) || end < offset {
		return nil, offset, ErrTruncatedBinary
	}
	body := buf[:end]
	count, n, err := leb128.LoadUint32(body[offset:])
	if err != nil {
		return nil, offset, fmt.Errorf("count: %w", err)
	}
	off := offset + int(n)
	defs := make([]CoreTypeDef, 0, min(int(count), end-off))
	for i := range count {
		var d CoreTypeDef
		if d, off, err = readCoreTypeDef(body, off); err != nil {
			return nil, off, fmt.Errorf("core type %d: %w", i, err)
		}
		defs = append(defs, d)
	}
	if off != end {
		return nil, off, fmt.Errorf("section size mismatch: %d bytes left", end-off)
	}
	return defs, off, nil
}

// readCoreTypeDef reads one core:type = core:deftype: a core:rectype, a
// non-final sub prefixed 0x00, or a core:moduletype (0x50).
func readCoreTypeDef(buf []byte, off int) (CoreTypeDef, int, error) {
	if off >= len(buf) {
		return CoreTypeDef{}, off, ErrTruncatedBinary
	}
	switch buf[off] {
	case 0x50: // core:moduletype
		mt, end, err := readCoreModuleType(buf, off+1)
		if err != nil {
			return CoreTypeDef{}, end, err
		}
		return CoreTypeDef{Module: mt, Count: 1}, end, nil
	case 0x00: // 0x00 0x50 x*:vec(typeidx) comptype: a non-final sub
		if off+1 >= len(buf) || buf[off+1] != 0x50 {
			return CoreTypeDef{}, off, fmt.Errorf("core:type: 0x00 must prefix a non-final sub (0x50)")
		}
		end, err := skipSubType(buf, off+1)
		if err != nil {
			return CoreTypeDef{}, end, err
		}
		return CoreTypeDef{Rec: buf[off+1 : end], Count: 1}, end, nil
	}
	count, end, err := skipRecType(buf, off)
	if err != nil {
		return CoreTypeDef{}, end, err
	}
	return CoreTypeDef{Rec: buf[off:end], Count: count}, end, nil
}

// readCoreModuleType reads vec(core:moduledecl), the body of a module type.
func readCoreModuleType(buf []byte, off int) (*CoreModuleType, int, error) {
	count, n, err := leb128.LoadUint32(buf[off:])
	if err != nil {
		return nil, off, fmt.Errorf("core:moduletype: %w", err)
	}
	off += int(n)
	mt := &CoreModuleType{Decls: make([]CoreModuleDecl, 0, min(int(count), len(buf)-off))}
	for i := range count {
		var d CoreModuleDecl
		if d, off, err = readCoreModuleDecl(buf, off); err != nil {
			return nil, off, fmt.Errorf("coremoduledecl[%d]: %w", i, err)
		}
		mt.Decls = append(mt.Decls, d)
	}
	return mt, off, nil
}

// readCoreModuleDecl reads one core:moduledecl: an import (0x00), a type
// (0x01, a core:rectype as a core type section encodes it -- a nested module
// type is not one), an outer type alias (0x02), or an export (0x03).
func readCoreModuleDecl(buf []byte, off int) (CoreModuleDecl, int, error) {
	if off >= len(buf) {
		return CoreModuleDecl{}, off, ErrTruncatedBinary
	}
	d := CoreModuleDecl{Kind: CoreModuleDeclKind(buf[off])}
	off++
	var err error
	switch d.Kind {
	case CoreModuleDeclImport:
		if d.Module, off, err = readCoreName(buf, off); err != nil {
			return d, off, err
		}
		if d.Name, off, err = readCoreName(buf, off); err != nil {
			return d, off, err
		}
		d.Extern, off, err = readCoreExternType(buf, off)
		return d, off, err
	case CoreModuleDeclType:
		start := off
		if d.RecCount, off, err = skipRecType(buf, off); err != nil {
			return d, off, err
		}
		d.Rec = buf[start:off]
		return d, off, nil
	case CoreModuleDeclAlias:
		// core:alias ::= 0x10 0x01 ct:u32 idx:u32 -- an outer type alias,
		// the only core alias a module type may declare.
		if off+1 >= len(buf) {
			return d, off, ErrTruncatedBinary
		}
		if buf[off] != 0x10 || buf[off+1] != 0x01 {
			return d, off, fmt.Errorf("core:alias: only an outer type alias (0x10 0x01) may appear in a module type, got %#x %#x", buf[off], buf[off+1])
		}
		off += 2
		if d.AliasCount, off, err = readU32(buf, off); err != nil {
			return d, off, err
		}
		d.AliasIndex, off, err = readU32(buf, off)
		return d, off, err
	case CoreModuleDeclExport:
		if d.Name, off, err = readCoreName(buf, off); err != nil {
			return d, off, err
		}
		d.Extern, off, err = readCoreExternType(buf, off)
		return d, off, err
	default:
		return d, off, fmt.Errorf("coremoduledecl: invalid tag %#x", byte(d.Kind))
	}
}

// readCoreExternType reads a core:externtype: func (0x00 typeidx), table
// (0x01 reftype limits), memory (0x02 limits), global (0x03 valtype mut) or
// tag (0x04 0x00 typeidx).
func readCoreExternType(buf []byte, off int) (CoreExternType, int, error) {
	if off >= len(buf) {
		return CoreExternType{}, off, ErrTruncatedBinary
	}
	x := CoreExternType{Kind: buf[off]}
	off++
	start := off
	var err error
	switch x.Kind {
	case 0x00: // func
		x.TypeIndex, off, err = readU32(buf, off)
	case 0x01: // table: reftype limits
		if off, err = skipValType(buf, off); err == nil {
			off, err = skipLimits(buf, off)
		}
	case 0x02: // memory: limits
		off, err = skipLimits(buf, off)
	case 0x03: // global: valtype mut
		if off, err = skipValType(buf, off); err == nil {
			if off >= len(buf) {
				return x, off, ErrTruncatedBinary
			}
			if buf[off] > 0x01 {
				return x, off, fmt.Errorf("core:globaltype: invalid mutability %#x", buf[off])
			}
			off++
		}
	case 0x04: // tag: attribute 0x00, then typeidx
		if off >= len(buf) {
			return x, off, ErrTruncatedBinary
		}
		if buf[off] != 0x00 {
			return x, off, fmt.Errorf("core:tagtype: invalid attribute %#x", buf[off])
		}
		x.TypeIndex, off, err = readU32(buf, off+1)
	default:
		return x, off, fmt.Errorf("core:externtype: invalid sort %#x", x.Kind)
	}
	if err != nil {
		return x, off, err
	}
	x.Bytes = buf[start:off]
	return x, off, nil
}

// skipRecType consumes a core:rectype -- a recursion group 0x4e
// vec(subtype), or a single subtype -- and returns how many types it
// defines.
func skipRecType(buf []byte, off int) (uint32, int, error) {
	if off >= len(buf) {
		return 0, off, ErrTruncatedBinary
	}
	if buf[off] != 0x4e {
		off, err := skipSubType(buf, off)
		return 1, off, err
	}
	count, off, err := readU32(buf, off+1)
	if err != nil {
		return 0, off, err
	}
	for i := uint32(0); i < count; i++ {
		if off, err = skipSubType(buf, off); err != nil {
			return 0, off, fmt.Errorf("rec group member %d: %w", i, err)
		}
	}
	return count, off, nil
}

// skipSubType consumes a core:subtype: 0x50 (non-final) or 0x4f (final)
// vec(typeidx) then a comptype, or a bare comptype.
func skipSubType(buf []byte, off int) (int, error) {
	if off >= len(buf) {
		return off, ErrTruncatedBinary
	}
	if b := buf[off]; b == 0x50 || b == 0x4f {
		count, o, err := readU32(buf, off+1)
		if err != nil {
			return o, err
		}
		off = o
		for range count {
			if _, off, err = readU32(buf, off); err != nil {
				return off, err
			}
		}
	}
	return skipCompType(buf, off)
}

// skipCompType consumes a core:comptype: func (0x60 resulttype resulttype),
// struct (0x5f vec(fieldtype)) or array (0x5e fieldtype).
func skipCompType(buf []byte, off int) (int, error) {
	if off >= len(buf) {
		return off, ErrTruncatedBinary
	}
	tag := buf[off]
	off++
	var err error
	switch tag {
	case 0x60: // func
		for range 2 {
			var count uint32
			if count, off, err = readU32(buf, off); err != nil {
				return off, err
			}
			for range count {
				if off, err = skipValType(buf, off); err != nil {
					return off, err
				}
			}
		}
		return off, nil
	case 0x5f: // struct
		var count uint32
		if count, off, err = readU32(buf, off); err != nil {
			return off, err
		}
		for range count {
			if off, err = skipFieldType(buf, off); err != nil {
				return off, err
			}
		}
		return off, nil
	case 0x5e: // array
		return skipFieldType(buf, off)
	default:
		return off, fmt.Errorf("core:comptype: invalid tag %#x", tag)
	}
}

// skipFieldType consumes a fieldtype: a storage type (a valtype, or i8 0x78
// / i16 0x77) and a mutability byte.
func skipFieldType(buf []byte, off int) (int, error) {
	if off >= len(buf) {
		return off, ErrTruncatedBinary
	}
	var err error
	if b := buf[off]; b == 0x78 || b == 0x77 {
		off++
	} else if off, err = skipValType(buf, off); err != nil {
		return off, err
	}
	if off >= len(buf) {
		return off, ErrTruncatedBinary
	}
	if buf[off] > 0x01 {
		return off, fmt.Errorf("core:fieldtype: invalid mutability %#x", buf[off])
	}
	return off + 1, nil
}

// skipValType consumes a core:valtype: a number or vector type, an
// abbreviated reference type, or 0x63/0x64 (ref null / ref) and a heap type.
func skipValType(buf []byte, off int) (int, error) {
	if off >= len(buf) {
		return off, ErrTruncatedBinary
	}
	b := buf[off]
	switch {
	case b >= 0x7b && b <= 0x7f: // i32 i64 f32 f64 v128
		return off + 1, nil
	case isAbsHeapType(b): // an abbreviated reftype, e.g. funcref
		return off + 1, nil
	case b == 0x63 || b == 0x64: // (ref null ht) / (ref ht)
		return skipHeapType(buf, off+1)
	default:
		return off, fmt.Errorf("core:valtype: invalid type %#x", b)
	}
}

// skipHeapType consumes a heap type: an abstract one (a single byte) or a
// type index as an s33.
func skipHeapType(buf []byte, off int) (int, error) {
	if off >= len(buf) {
		return off, ErrTruncatedBinary
	}
	if isAbsHeapType(buf[off]) {
		return off + 1, nil
	}
	v, n, err := leb128.LoadInt33AsInt64(buf[off:])
	if err != nil {
		return off, fmt.Errorf("core:heaptype: %w", err)
	}
	if v < 0 {
		return off, fmt.Errorf("core:heaptype: invalid type %d", v)
	}
	return off + int(n), nil
}

// isAbsHeapType reports whether b is an abstract heap type -- and so, as a
// valtype, the abbreviated nullable reference to it: exn 0x69, array 0x6a,
// struct 0x6b, i31 0x6c, eq 0x6d, any 0x6e, extern 0x6f, func 0x70, none
// 0x71, noextern 0x72, nofunc 0x73, noexn 0x74.
func isAbsHeapType(b byte) bool { return b >= 0x69 && b <= 0x74 }

// skipLimits consumes a limits: a flags byte (bit 0 a maximum, bit 1
// shared, bit 2 64-bit indices, bit 3 a custom page size), the minimum, the
// maximum when present and the page size's log2 when custom.
func skipLimits(buf []byte, off int) (int, error) {
	if off >= len(buf) {
		return off, ErrTruncatedBinary
	}
	flags := buf[off]
	if flags > 0x0f {
		return off, fmt.Errorf("core:limits: invalid flags %#x", flags)
	}
	off++
	var err error
	if off, err = skipU64(buf, off); err != nil {
		return off, err
	}
	if flags&0x01 != 0 {
		if off, err = skipU64(buf, off); err != nil {
			return off, err
		}
	}
	if flags&0x08 != 0 {
		if _, off, err = readU32(buf, off); err != nil {
			return off, err
		}
	}
	return off, nil
}

// readCoreName reads a core:name: a length and that many UTF-8 bytes.
func readCoreName(buf []byte, off int) (string, int, error) {
	s, end, err := readLabel(buf, off)
	if err != nil {
		return "", end, fmt.Errorf("core:name: %w", err)
	}
	return s, end, nil
}

// readU32 reads a u32 LEB128.
func readU32(buf []byte, off int) (uint32, int, error) {
	if off >= len(buf) {
		return 0, off, ErrTruncatedBinary
	}
	v, n, err := leb128.LoadUint32(buf[off:])
	if err != nil {
		return 0, off, err
	}
	return v, off + int(n), nil
}

// skipU64 consumes a u64 LEB128.
func skipU64(buf []byte, off int) (int, error) {
	if off >= len(buf) {
		return off, ErrTruncatedBinary
	}
	_, n, err := leb128.LoadUint64(buf[off:])
	if err != nil {
		return off, err
	}
	return off + int(n), nil
}

// CoreModuleExports calls fn with each export of a core module -- its name,
// which aliases the module's bytes, and its sort -- reading the export
// section alone: every other section, the code included, is stepped over by
// its size. It is all a check of export names needs, and allocates nothing.
func CoreModuleExports(module []byte, fn func(name []byte, kind byte)) error {
	if len(module) < 8 {
		return ErrTruncatedBinary
	}
	off := 8 // magic and version
	for off < len(module) {
		id := module[off]
		size, end, err := readU32(module, off+1)
		if err != nil {
			return fmt.Errorf("section %d: %w", id, err)
		}
		if uint64(end)+uint64(size) > uint64(len(module)) {
			return fmt.Errorf("section %d: %w", id, ErrTruncatedBinary)
		}
		body := module[:end+int(size)]
		off = end + int(size)
		if id != 7 {
			continue
		}
		count, p, err := readU32(body, end)
		if err != nil {
			return fmt.Errorf("section 7: %w", err)
		}
		for i := range count {
			var n uint32
			if n, p, err = readU32(body, p); err != nil {
				return fmt.Errorf("section 7 entry %d: %w", i, err)
			}
			if uint64(p)+uint64(n) >= uint64(len(body)) {
				return fmt.Errorf("section 7 entry %d: %w", i, ErrTruncatedBinary)
			}
			name, kind := body[p:p+int(n)], body[p+int(n)]
			if _, p, err = readU32(body, p+int(n)+1); err != nil {
				return fmt.Errorf("section 7 entry %d: %w", i, err)
			}
			fn(name, kind)
		}
	}
	return nil
}
