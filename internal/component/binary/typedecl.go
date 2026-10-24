package binary

import (
	"fmt"

	"github.com/samyfodil/wazy/internal/leb128"
)

// This file reads the declaration list of a component type (0x41) or an
// instance type (0x42): every declaration, which a type check needs, and
// the type's own type index space, which the Canonical ABI resolves value
// types through (see InstanceDesc).

// TypeDeclKind says what one declaration of a component or instance type is.
// Its values are the declaration tags of the binary format.
type TypeDeclKind byte

const (
	// TypeDeclCoreType is a core type definition (core:type): TypeDecl.CoreType.
	// It opens the next index of the type's own core type index space, not of
	// a component-level one.
	TypeDeclCoreType TypeDeclKind = 0x00
	// TypeDeclType is a type definition: TypeDecl.Type.
	TypeDeclType TypeDeclKind = 0x01
	// TypeDeclAlias is an alias: TypeDecl.Alias.
	TypeDeclAlias TypeDeclKind = 0x02
	// TypeDeclImport is an import, which only a component type has:
	// TypeDecl.Name and TypeDecl.Desc.
	TypeDeclImport TypeDeclKind = 0x03
	// TypeDeclExport is an export: TypeDecl.Name and TypeDecl.Desc.
	TypeDeclExport TypeDeclKind = 0x04
)

// TypeDecl is one declaration of a component or instance type, in the order
// the type declares them. A type opens index spaces of its own (see
// InstanceDesc), so every index a declaration carries -- in Type, in Alias,
// in Desc -- names an item of the enclosing type's own spaces, unless an
// outer alias put it there.
type TypeDecl struct {
	Kind TypeDeclKind

	// Type is the definition of a TypeDeclType.
	Type TypeDesc

	// Alias is the alias of a TypeDeclAlias.
	Alias AliasDef

	// CoreType is the definition of a TypeDeclCoreType.
	CoreType *CoreTypeDef

	// Name and Desc are the name and the declared type of a TypeDeclImport
	// or TypeDeclExport.
	Name string
	Desc ExternDesc
}

// typeBody is what reading a component or instance type's declarations
// builds: the fields of an InstanceDesc or ComponentDesc.
type typeBody struct {
	decls   []TypeDecl
	types   []TypeDesc
	exports map[string]TypeRef
	outer   map[uint32]OuterAlias
}

// readTypeBody reads vec(instancedecl), or vec(componentdecl) when component
// is set: the same productions but for the import declaration, which only a
// component type may have.
func readTypeBody(buf []byte, off int, component bool) (typeBody, int, error) {
	var b typeBody
	count, n, err := leb128.LoadUint32(buf[off:])
	if err != nil {
		return b, off, err
	}
	off += int(n)
	// Every declaration consumes at least one byte, so both count and the
	// bytes left bound the declarations -- take the smaller, so a bogus
	// LEB128 count cannot make this allocate before the read that refuses
	// it. Only some declarations add a type, but most do.
	if c := min(int(count), len(buf)-off); c > 0 {
		b.decls = make([]TypeDecl, 0, c)
		b.types = make([]TypeDesc, 0, c)
	}
	what := "instancedecl"
	if component {
		what = "componentdecl"
	}
	for i := range count {
		if off, err = b.read(buf, off, component); err != nil {
			return b, off, fmt.Errorf("%s[%d]: %w", what, i, err)
		}
	}
	return b, off, nil
}

// read consumes one declaration into b.
//
// A type definition, a type-sort alias, and a type-sort import or export
// each add the next index of the type's own type index space, in
// declaration order -- as the enclosing component's TypeSpace is built (see
// typespace.go), just scoped to the type. Only a definition adds a type
// this decoder resolves structurally; every other slot is nil, or what an
// `eq` bound names, but is still there, or a later declaration naming a
// slot by number would get the wrong type rather than none.
func (b *typeBody) read(buf []byte, off int, component bool) (int, error) {
	if off >= len(buf) {
		return off, ErrTruncatedBinary
	}
	d := TypeDecl{Kind: TypeDeclKind(buf[off])}
	off++
	var err error
	switch d.Kind {
	case TypeDeclType:
		if d.Type, off, err = readDeftypeDesc(buf, off); err != nil {
			return off, err
		}
		b.types = append(b.types, d.Type)
	case TypeDeclImport, TypeDeclExport:
		if d.Kind == TypeDeclImport && !component {
			return off, fmt.Errorf("instancedecl: invalid tag %#x", byte(d.Kind))
		}
		if d.Name, off, err = readExternName(buf, off); err != nil {
			return off, err
		}
		if d.Desc, off, err = readExternDesc(buf, off); err != nil {
			return off, err
		}
		if d.Desc.Sort == 0x03 {
			if d.Desc.Eq && !component {
				// An `eq N` export of an instance type is local type N under
				// a name, which the ABI resolves through Exports. Built on the
				// first: most instance types export only funcs.
				if b.exports == nil {
					b.exports = make(map[string]TypeRef, 4)
				}
				idx := d.Desc.Index
				b.exports[d.Name] = TypeRef{TypeIndex: &idx}
			}
			addLocalType(&b.types, &b.outer, d.Desc.Index, d.Desc.Eq)
		}
	case TypeDeclAlias:
		if d.Alias, off, err = readAliasDef(buf, off); err != nil {
			return off, err
		}
		if d.Alias.Sort == 0x03 {
			// This decoder does not follow an alias structurally (see
			// resolveAlias); an outer one is recorded for what it names.
			if d.Alias.TargetKind == 0x02 {
				if b.outer == nil {
					b.outer = map[uint32]OuterAlias{}
				}
				b.outer[uint32(len(b.types))] = OuterAlias{d.Alias.OuterCount, d.Alias.OuterIndex}
			}
			b.types = append(b.types, nil)
		}
	case TypeDeclCoreType:
		// The type's CORE type index space, apart from the one types is.
		ct, off2, err := readCoreTypeDef(buf, off)
		if err != nil {
			return off2, err
		}
		off, d.CoreType = off2, &ct
	default:
		return off, fmt.Errorf("instancedecl: invalid tag %#x", byte(d.Kind))
	}
	b.decls = append(b.decls, d)
	return off, nil
}
