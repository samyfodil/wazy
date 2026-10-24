package binary

import (
	"fmt"

	"github.com/samyfodil/wazy/internal/leb128"
)

// This file parses the declaration list of a component type (0x41) or an
// instance type (0x42) in full. Decoding a component keeps only what the
// Canonical ABI needs of these -- an instance type's type-sort exports (see
// InstanceDesc), nothing at all of a component type -- and the body bytes;
// a type check that needs every import and export a type declares parses
// the body again through here. See InstanceDesc.Body.

// TypeDeclKind says what one declaration of a component or instance type is.
// Its values are the declaration tags of the binary format.
type TypeDeclKind byte

const (
	// TypeDeclCoreType is a core type definition (core:type), opening no
	// component-level index.
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

	// Name and Desc are the name and the declared type of a TypeDeclImport
	// or TypeDeclExport.
	Name string
	Desc ExternDesc
}

// Decls parses the instance type's declarations.
func (d InstanceDesc) Decls() ([]TypeDecl, error) {
	return readTypeDecls(d.Body, false)
}

// Decls parses the component type's declarations.
func (d ComponentDesc) Decls() ([]TypeDecl, error) {
	return readTypeDecls(d.Body, true)
}

// readTypeDecls parses vec(componentdecl), or vec(instancedecl) when
// component is false: the same productions but for the import declaration,
// which only a component type may have.
func readTypeDecls(body []byte, component bool) ([]TypeDecl, error) {
	count, n, err := leb128.LoadUint32(body)
	if err != nil {
		return nil, fmt.Errorf("type declarations: %w", err)
	}
	off := int(n)
	// Every declaration takes at least one byte, so the bytes left bound the
	// count; a bogus LEB128 count then cannot make this allocate up front.
	decls := make([]TypeDecl, 0, min(int(count), len(body)-off))
	for i := range count {
		var d TypeDecl
		if d, off, err = readTypeDecl(body, off, component); err != nil {
			return nil, fmt.Errorf("declaration %d: %w", i, err)
		}
		decls = append(decls, d)
	}
	if off != len(body) {
		return nil, fmt.Errorf("type declarations: %d trailing bytes", len(body)-off)
	}
	return decls, nil
}

func readTypeDecl(buf []byte, off int, component bool) (TypeDecl, int, error) {
	if off >= len(buf) {
		return TypeDecl{}, off, ErrTruncatedBinary
	}
	d := TypeDecl{Kind: TypeDeclKind(buf[off])}
	off++
	var err error
	switch d.Kind {
	case TypeDeclCoreType:
		off, err = readCoretypeDef(buf, off)
	case TypeDeclType:
		d.Type, off, err = readDeftypeDesc(buf, off)
	case TypeDeclAlias:
		d.Alias, off, err = readAliasDef(buf, off)
	case TypeDeclImport, TypeDeclExport:
		if d.Kind == TypeDeclImport && !component {
			return d, off, fmt.Errorf("an instance type cannot declare an import")
		}
		if d.Name, off, err = readExternName(buf, off); err == nil {
			d.Desc, off, err = readExternDesc(buf, off)
		}
	default:
		return d, off, fmt.Errorf("invalid declaration tag %#x", byte(d.Kind))
	}
	return d, off, err
}
