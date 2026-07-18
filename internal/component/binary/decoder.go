package binary

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/samyfodil/wazy/internal/leb128"
)

// Component preamble constants (per WebAssembly/component-model spec).
var (
	// Magic number is the same as core wasm modules.
	magic = []byte{0x00, 0x61, 0x73, 0x6d}

	// Component version distinguishes a component from a core module.
	componentVersion = []byte{0x0d, 0x00}

	// Layer byte: 0x01 0x00 means this is a component, not a core module (which has 0x00 0x00).
	componentLayer = []byte{0x01, 0x00}
)

var (
	ErrInvalidMagicNumber = errors.New("invalid magic number")
	ErrInvalidVersion     = errors.New("invalid version header")
	ErrInvalidLayer       = errors.New("invalid layer header (expected component layer 0x01 0x00)")
	ErrInvalidSectionID   = errors.New("invalid section id")
	ErrTruncatedBinary    = errors.New("truncated binary")
)

// Decode parses a WebAssembly Component Model container from a reader.
// It validates the preamble, iterates outer component sections, and decodes
// type, import, and export sections into Go structs. Other sections are
// recorded but not fully decoded.
func Decode(r io.Reader) (*Component, error) {
	// Read the entire binary into memory for slice-based offset tracking.
	// (Matching the pattern of the core wasm decoder.)
	buf, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read binary: %w", err)
	}

	return decodeComponent(buf)
}

func decodeComponent(buf []byte) (*Component, error) {
	offset := 0

	// Validate magic number.
	if len(buf) < 4 || !bytes.Equal(buf[:4], magic) {
		return nil, ErrInvalidMagicNumber
	}
	offset = 4

	// Validate component version (0x0d 0x00).
	if len(buf) < offset+2 || !bytes.Equal(buf[offset:offset+2], componentVersion) {
		return nil, ErrInvalidVersion
	}
	offset += 2

	// Validate component layer (0x01 0x00).
	if len(buf) < offset+2 || !bytes.Equal(buf[offset:offset+2], componentLayer) {
		return nil, ErrInvalidLayer
	}
	offset += 2

	c := &Component{}

	// Parse sections. Unlike core wasm, component sections may appear in any
	// order and the same section id may occur multiple times (e.g. several
	// type sections interleaved with module/instance/alias sections), so there
	// is no section-order constraint to enforce and results accumulate.
	for offset < len(buf) {
		sectionID := buf[offset]
		offset++

		// Read section size as LEB128.
		sectionSize, n, err := leb128.LoadUint32(buf[offset:])
		if err != nil {
			return nil, fmt.Errorf("section %s: read size: %w", sectionIDName(sectionID), err)
		}
		offset += int(n)

		sectionStart := offset

		// Dispatch on section ID.
		switch sectionID {
		case 0: // Custom
			// Skip custom sections entirely for M1. Bounds-check the claimed
			// size against the remaining buffer so a truncated custom
			// section fails loud instead of silently truncating the
			// component (the section-size mismatch check below can't catch
			// this case, since it compares bytesRead against sectionSize,
			// and here bytesRead is unconditionally set equal to sectionSize).
			if sectionSize > uint32(len(buf)-offset) {
				return nil, fmt.Errorf("section %s: %w", sectionIDName(sectionID), ErrTruncatedBinary)
			}
			offset += int(sectionSize)

		case 1: // Core Module section
			newOffset, err := decodeCoreModuleSection(buf, offset, sectionStart, sectionSize)
			if err != nil {
				return nil, fmt.Errorf("core module section: %w", err)
			}
			offset = newOffset
			// Record the module's byte range
			c.CoreModules = append(c.CoreModules, CoreModule{
				Offset: sectionStart,
				Size:   int(sectionSize),
			})

		case 2: // Core Instance section
			coreInstances, newOffset, err := decodeCoreInstanceSection(buf, offset, sectionSize)
			if err != nil {
				return nil, fmt.Errorf("core instance section: %w", err)
			}
			offset = newOffset
			c.CoreInstances = append(c.CoreInstances, coreInstances...)

		case 3: // Core Type section (not yet fully decoded)
			if sectionSize > uint32(len(buf)-offset) {
				return nil, fmt.Errorf("section %s: %w", sectionIDName(sectionID), ErrTruncatedBinary)
			}
			c.RawSections = append(c.RawSections, RawSection{ID: sectionID, Size: sectionSize})
			offset += int(sectionSize)

		case 4: // Component section: a fully embedded nested component.
			if sectionSize > uint32(len(buf)-offset) {
				return nil, fmt.Errorf("section %s: %w", sectionIDName(sectionID), ErrTruncatedBinary)
			}
			// Per Binary.md, section_4(<component>) carries a *complete*
			// component binary (preamble included), so it recurses through
			// decodeComponent unchanged rather than through Decode (which
			// would re-read the whole buffer via io.ReadAll).
			nested, err := decodeComponent(buf[offset : offset+int(sectionSize)])
			if err != nil {
				return nil, fmt.Errorf("nested component[%d]: %w", len(c.NestedComponents), err)
			}
			c.NestedComponents = append(c.NestedComponents, nested)
			offset += int(sectionSize)

		case 5: // Instance section
			instances, newOffset, err := decodeInstanceSection(buf, offset, sectionSize)
			if err != nil {
				return nil, fmt.Errorf("instance section: %w", err)
			}
			offset = newOffset
			c.Instances = append(c.Instances, instances...)

		case 6: // Alias section
			aliases, newOffset, err := decodeAliasSection(buf, offset, sectionSize)
			if err != nil {
				return nil, fmt.Errorf("alias section: %w", err)
			}
			offset = newOffset
			aliasBase := uint32(len(c.Aliases))
			c.Aliases = append(c.Aliases, aliases...)
			for j, al := range aliases {
				// Type-sort aliases (sort 0x03) occupy the next index in the
				// component's full type index space, interleaved with
				// type-section deftypes and imported types -- see typespace.go.
				if al.Sort == 0x03 {
					c.TypeSpace = append(c.TypeSpace, TypeSpaceEntry{Kind: TypeSpaceAlias, Alias: aliasBase + uint32(j)})
				}
				// A core-level func alias (sort 0x00 core, core:sort 0x00
				// func) occupies the next index in the component's core func
				// index space, interleaved with canon-produced core funcs --
				// see corefuncspace.go.
				if al.Sort == 0x00 && al.CoreSort == 0x00 {
					c.CoreFuncSpace = append(c.CoreFuncSpace, CoreFuncSpaceEntry{Kind: CoreFuncFromAlias, Alias: aliasBase + uint32(j)})
				}
				// A component-level func alias (sort 0x01) occupies the next
				// index in the component func index space -- see
				// componentfuncspace.go.
				if al.Sort == 0x01 {
					c.ComponentFuncSpace = append(c.ComponentFuncSpace, ComponentFuncSpaceEntry{Kind: ComponentFuncFromAlias, Alias: aliasBase + uint32(j)})
				}
			}

		case 7: // Type section
			types, newOffset, err := decodeTypeSection(buf, offset, sectionSize)
			if err != nil {
				return nil, fmt.Errorf("type section: %w", err)
			}
			offset = newOffset
			base := uint32(len(c.Types))
			for j := range types {
				types[j].Index = base + uint32(j)
				// Every type-section deftype occupies the next index in the
				// full type index space -- see typespace.go.
				c.TypeSpace = append(c.TypeSpace, TypeSpaceEntry{Kind: TypeSpaceDef, Def: base + uint32(j)})
			}
			c.Types = append(c.Types, types...)

		case 8: // Canonical section
			canons, newOffset, err := decodeCanonSection(buf, offset, sectionSize)
			if err != nil {
				return nil, fmt.Errorf("canon section: %w", err)
			}
			offset = newOffset
			canonBase := uint32(len(c.Canons))
			c.Canons = append(c.Canons, canons...)
			// Every canon EXCEPT lift produces a new core func (lower, the
			// three resource canons, and every async builtin: task.*,
			// subtask.*, context.get/set, stream.*, future.*, error-context.*,
			// waitable*, backpressure.*), occupying the next index in the
			// component's core func index space, interleaved with core-level
			// func aliases -- see corefuncspace.go. Only lift produces a new
			// COMPONENT func instead (componentfuncspace.go).
			for j, cn := range canons {
				if cn.Kind == CanonKindLift {
					c.ComponentFuncSpace = append(c.ComponentFuncSpace, ComponentFuncSpaceEntry{Kind: ComponentFuncFromCanonLift, Canon: canonBase + uint32(j)})
				} else {
					c.CoreFuncSpace = append(c.CoreFuncSpace, CoreFuncSpaceEntry{Kind: CoreFuncFromCanon, Canon: canonBase + uint32(j)})
				}
			}

		case 9: // Start section
			start, newOffset, err := decodeStartSection(buf, offset, sectionSize)
			if err != nil {
				return nil, fmt.Errorf("start section: %w", err)
			}
			offset = newOffset
			c.Start = start

		case 10: // Import section
			imports, newOffset, err := decodeImportSection(buf, offset, sectionSize)
			if err != nil {
				return nil, fmt.Errorf("import section: %w", err)
			}
			offset = newOffset
			importBase := uint32(len(c.Imports))
			c.Imports = append(c.Imports, imports...)
			// An import whose externdesc names a type (0x03) occupies the
			// next index in the component's full type index space -- see
			// typespace.go. Its structural definition is not decoded (see
			// ResolveType), but its position in the space must still be
			// accounted for so later deftypes/aliases get the right index.
			for j, im := range imports {
				if im.ExternType == 0x03 {
					c.TypeSpace = append(c.TypeSpace, TypeSpaceEntry{Kind: TypeSpaceImport, Import: importBase + uint32(j)})
				}
				// A func import (sort func 0x01) occupies the next index in the
				// component func index space -- see componentfuncspace.go.
				if im.ExternType == 0x01 {
					c.ComponentFuncSpace = append(c.ComponentFuncSpace, ComponentFuncSpaceEntry{Kind: ComponentFuncFromImport, Import: importBase + uint32(j)})
				}
			}

		case 11: // Export section
			exports, newOffset, err := decodeExportSection(buf, offset, sectionSize)
			if err != nil {
				return nil, fmt.Errorf("export section: %w", err)
			}
			offset = newOffset
			exportBase := uint32(len(c.Exports))
			c.Exports = append(c.Exports, exports...)
			// Exporting a func (sort func 0x01) creates a new component func
			// index aliasing the exported func -- see componentfuncspace.go.
			for j, ex := range exports {
				switch ex.ExternType {
				case 0x01: // func export: introduces a component func index
					c.ComponentFuncSpace = append(c.ComponentFuncSpace, ComponentFuncSpaceEntry{Kind: ComponentFuncFromExport, Export: exportBase + uint32(j)})
				case 0x03: // type export: introduces a type index aliasing the
					// exported type ("export introduces an alias").
					c.TypeSpace = append(c.TypeSpace, TypeSpaceEntry{Kind: TypeSpaceExport, Export: exportBase + uint32(j)})
				}
			}

		default:
			// Record and skip unknown/unimplemented sections. Same
			// truncation bounds-check as the custom-section case above.
			if sectionSize > uint32(len(buf)-offset) {
				return nil, fmt.Errorf("section %s: %w", sectionIDName(sectionID), ErrTruncatedBinary)
			}
			c.RawSections = append(c.RawSections, RawSection{ID: sectionID, Size: sectionSize})
			offset += int(sectionSize)
		}

		// Verify we consumed exactly the right number of bytes.
		bytesRead := offset - sectionStart
		if int(sectionSize) != bytesRead {
			return nil, fmt.Errorf("section %s: expected %d bytes but read %d", sectionIDName(sectionID), sectionSize, bytesRead)
		}
	}

	c.Decoded = true
	c.Bytes = buf
	return c, nil
}

// decodeTypeSection decodes the type section (section id 7): vec(deftype).
// Each entry is walked structurally so following sections stay byte-aligned;
// the type's kind is recorded for the dump.
func decodeTypeSection(buf []byte, offset int, sectionSize uint32) ([]Type, int, error) {
	sectionStart := offset

	count, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return nil, offset, fmt.Errorf("read count: %w", err)
	}
	offset += int(n)

	types := make([]Type, count)
	for i := range count {
		// Single walk: the descriptor is the source of truth, and its Kind()
		// method derives the human-readable kind string.
		desc, newOffset, err := readDeftypeDesc(buf, offset)
		if err != nil {
			return nil, newOffset, fmt.Errorf("type[%d]: %w", i, err)
		}
		offset = newOffset
		types[i] = Type{Index: i, Kind: desc.Kind(), Descriptor: desc}
	}

	if bytesRead := offset - sectionStart; bytesRead > int(sectionSize) {
		return nil, offset, fmt.Errorf("type section: read %d bytes but section is only %d", bytesRead, sectionSize)
	}
	return types, offset, nil
}

// decodeImportSection decodes the import section (section id 10): vec(import),
// where import = externname externdesc.
func decodeImportSection(buf []byte, offset int, sectionSize uint32) ([]Import, int, error) {
	sectionStart := offset

	count, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return nil, offset, fmt.Errorf("read count: %w", err)
	}
	offset += int(n)

	imports := make([]Import, count)
	for i := range count {
		name, off, err := readExternName(buf, offset)
		if err != nil {
			return nil, off, fmt.Errorf("import[%d] name: %w", i, err)
		}
		sort, eqIdx, hasEq, off2, err := readExterndesc(buf, off)
		if err != nil {
			return nil, off2, fmt.Errorf("import[%d] externdesc: %w", i, err)
		}
		offset = off2
		imports[i] = Import{Name: name, ExternType: sort}
		switch {
		case sort == 0x03 && hasEq: // type import with an `eq N` bound
			imports[i].TypeEqIndex = eqIdx
			imports[i].TypeEqBound = true
		case sort == 0x01 && hasEq: // func import: eqIdx is the func's own type index
			imports[i].ExternIndex = eqIdx
		}
	}

	if bytesRead := offset - sectionStart; bytesRead > int(sectionSize) {
		return nil, offset, fmt.Errorf("import section: read %d bytes but section is only %d", bytesRead, sectionSize)
	}
	return imports, offset, nil
}

// decodeExportSection decodes the export section (section id 11): vec(export),
// where export = externname sortidx opt(externdesc). The optional ascribed
// externdesc is not decoded in M1; the per-section size check catches any
// export that carries one.
func decodeExportSection(buf []byte, offset int, sectionSize uint32) ([]Export, int, error) {
	sectionStart := offset

	count, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return nil, offset, fmt.Errorf("read count: %w", err)
	}
	offset += int(n)

	exports := make([]Export, count)
	for i := range count {
		name, off, err := readExternName(buf, offset)
		if err != nil {
			return nil, off, fmt.Errorf("export[%d] name: %w", i, err)
		}
		sort, idx, off2, err := readSortidx(buf, off)
		if err != nil {
			return nil, off2, fmt.Errorf("export[%d] sortidx: %w", i, err)
		}
		// Optional ascribed type: 0x00 (none) or 0x01 externdesc.
		if off2 >= len(buf) {
			return nil, off2, ErrTruncatedBinary
		}
		hasType := buf[off2]
		off2++
		switch hasType {
		case 0x00:
			// no ascribed type
		case 0x01:
			if _, _, _, off2, err = readExterndesc(buf, off2); err != nil {
				return nil, off2, fmt.Errorf("export[%d] ascribed type: %w", i, err)
			}
		default:
			return nil, off2, fmt.Errorf("export[%d]: invalid type-ascription tag %#x", i, hasType)
		}
		offset = off2
		exports[i] = Export{Name: name, ExternType: sort, ExternIndex: idx}
	}

	if bytesRead := offset - sectionStart; bytesRead > int(sectionSize) {
		return nil, offset, fmt.Errorf("export section: read %d bytes but section is only %d", bytesRead, sectionSize)
	}
	return exports, offset, nil
}

// readSortidx consumes a sortidx: a sort byte (core sort 0x00 carries an extra
// core-sort byte) followed by a u32 index.
func readSortidx(buf []byte, off int) (sort byte, idx uint32, _ int, err error) {
	if off >= len(buf) {
		return 0, 0, off, ErrTruncatedBinary
	}
	sort = buf[off]
	off++
	if sort == 0x00 { // core sort: one more discriminator byte
		if off >= len(buf) {
			return sort, 0, off, ErrTruncatedBinary
		}
		off++
	}
	idx, n, err := leb128.LoadUint32(buf[off:])
	if err != nil {
		return sort, 0, off, err
	}
	return sort, idx, off + int(n), nil
}

// decodeCoreModuleSection decodes the core module section (section 1).
// It just validates that the embedded core module is present; the actual
// wasm module is not parsed here (wazy's core decoder handles it).
func decodeCoreModuleSection(buf []byte, offset int, sectionStart int, sectionSize uint32) (int, error) {
	// Section 1 contains a single embedded core wasm module.
	// We store it by its byte range but don't decode it here.
	// Just advance the offset by sectionSize.
	return offset + int(sectionSize), nil
}

// decodeCoreInstanceSection decodes the core instance section (section 2).
// Section 2 contains vec(core:instance).
func decodeCoreInstanceSection(buf []byte, offset int, sectionSize uint32) ([]CoreInstance, int, error) {
	sectionStart := offset

	count, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return nil, offset, fmt.Errorf("read count: %w", err)
	}
	offset += int(n)

	instances := make([]CoreInstance, count)
	for i := range count {
		if offset >= len(buf) {
			return nil, offset, ErrTruncatedBinary
		}
		kind := buf[offset]
		offset++

		var instance CoreInstance
		instance.Kind = kind

		switch kind {
		case 0x00: // instantiate: moduleidx vec(core:instantiatearg)
			moduleIdx, n, err := leb128.LoadUint32(buf[offset:])
			if err != nil {
				return nil, offset, fmt.Errorf("instance[%d] module index: %w", i, err)
			}
			offset += int(n)
			instance.ModuleIdx = moduleIdx

			// Read args: vec(core:instantiatearg)
			argCount, n, err := leb128.LoadUint32(buf[offset:])
			if err != nil {
				return nil, offset, fmt.Errorf("instance[%d] arg count: %w", i, err)
			}
			offset += int(n)

			args := make([]CoreInstantiateArg, argCount)
			for j := range argCount {
				// A core instantiate-arg name is a plain core:name (label),
				// not an externname; see the core inline-export note above.
				name, off, err := readLabel(buf, offset)
				if err != nil {
					return nil, off, fmt.Errorf("instance[%d] arg[%d] name: %w", i, j, err)
				}
				offset = off

				// Each arg has a 0x12 prefix byte (the instance sort)
				if offset >= len(buf) || buf[offset] != 0x12 {
					return nil, offset, fmt.Errorf("instance[%d] arg[%d]: expected 0x12 prefix", i, j)
				}
				offset++

				instanceIdx, n, err := leb128.LoadUint32(buf[offset:])
				if err != nil {
					return nil, offset, fmt.Errorf("instance[%d] arg[%d] instance index: %w", i, j, err)
				}
				offset += int(n)

				args[j] = CoreInstantiateArg{Name: name, InstanceIdx: instanceIdx}
			}
			instance.Args = args

		case 0x01: // inline exports: vec(core:inlineexport)
			exportCount, n, err := leb128.LoadUint32(buf[offset:])
			if err != nil {
				return nil, offset, fmt.Errorf("instance[%d] export count: %w", i, err)
			}
			offset += int(n)

			exports := make([]CoreInlineExport, exportCount)
			for j := range exportCount {
				// A core inline-export name is a plain core:name (label), not
				// an import/export externname (which carries a 0x00/0x01 kind
				// byte). Using readExternName here mis-reads the label's length
				// prefix as a kind byte and rejects the whole section.
				name, off, err := readLabel(buf, offset)
				if err != nil {
					return nil, off, fmt.Errorf("instance[%d] export[%d] name: %w", i, j, err)
				}
				offset = off

				// A core inline export carries a core:sortidx: a single
				// core:sort byte (0x00 func, 0x01 table, 0x02 memory, 0x03
				// global, ...) followed by a u32 index. This is NOT the
				// component-level sortidx that readSortidx parses (which
				// prefixes the core sort with a 0x00 discriminator); using
				// readSortidx here over-reads the section by one byte.
				if offset >= len(buf) {
					return nil, offset, ErrTruncatedBinary
				}
				coreSort := buf[offset]
				offset++
				idx, n, err := leb128.LoadUint32(buf[offset:])
				if err != nil {
					return nil, offset, fmt.Errorf("instance[%d] export[%d] index: %w", i, j, err)
				}
				offset += int(n)

				exports[j] = CoreInlineExport{Name: name, Sort: coreSort, CoreSortIdx: idx}
			}
			instance.Exports = exports

		default:
			return nil, offset, fmt.Errorf("instance[%d]: invalid kind %#x", i, kind)
		}

		instances[i] = instance
	}

	if bytesRead := offset - sectionStart; bytesRead > int(sectionSize) {
		return nil, offset, fmt.Errorf("core instance section: read %d bytes but section is only %d", bytesRead, sectionSize)
	}
	return instances, offset, nil
}

// decodeInstanceSection decodes the instance section (section 5).
// Section 5 contains vec(instance).
func decodeInstanceSection(buf []byte, offset int, sectionSize uint32) ([]Instance, int, error) {
	sectionStart := offset

	count, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return nil, offset, fmt.Errorf("read count: %w", err)
	}
	offset += int(n)

	instances := make([]Instance, count)
	for i := range count {
		if offset >= len(buf) {
			return nil, offset, ErrTruncatedBinary
		}
		kind := buf[offset]
		offset++

		var instance Instance
		instance.Kind = kind

		switch kind {
		case 0x00: // instantiate: componentidx vec(instantiatearg)
			componentIdx, n, err := leb128.LoadUint32(buf[offset:])
			if err != nil {
				return nil, offset, fmt.Errorf("instance[%d] component index: %w", i, err)
			}
			offset += int(n)
			instance.ComponentIdx = componentIdx

			// Read args: vec(instantiatearg)
			argCount, n, err := leb128.LoadUint32(buf[offset:])
			if err != nil {
				return nil, offset, fmt.Errorf("instance[%d] arg count: %w", i, err)
			}
			offset += int(n)

			args := make([]InstantiateArg, argCount)
			for j := range argCount {
				// A component instantiate-arg name is `instantiatearg ::= n:<name>
				// si:<sortidx>` where `name ::= n:<core:name>` -- i.e. a plain
				// label (len + utf8), NOT an externname (which carries a
				// 0x00/0x01/0x02 kind-byte prefix). Same class of fix as the
				// core:instantiatearg case above; using readExternName here
				// mis-reads the label's length-prefix low byte as a kind byte.
				name, off, err := readLabel(buf, offset)
				if err != nil {
					return nil, off, fmt.Errorf("instance[%d] arg[%d] name: %w", i, j, err)
				}
				offset = off

				sort, idx, off, err := readSortidx(buf, offset)
				if err != nil {
					return nil, off, fmt.Errorf("instance[%d] arg[%d] sortidx: %w", i, j, err)
				}
				offset = off

				args[j] = InstantiateArg{Name: name, Sort: sort, SortIdx: idx}
			}
			instance.Args = args

		case 0x01: // inline exports: vec(inlineexport)
			exportCount, n, err := leb128.LoadUint32(buf[offset:])
			if err != nil {
				return nil, offset, fmt.Errorf("instance[%d] export count: %w", i, err)
			}
			offset += int(n)

			exports := make([]InlineExport, exportCount)
			for j := range exportCount {
				name, off, err := readExternName(buf, offset)
				if err != nil {
					return nil, off, fmt.Errorf("instance[%d] export[%d] name: %w", i, j, err)
				}
				offset = off

				sort, idx, off, err := readSortidx(buf, offset)
				if err != nil {
					return nil, off, fmt.Errorf("instance[%d] export[%d] sortidx: %w", i, j, err)
				}
				offset = off

				exports[j] = InlineExport{Name: name, Sort: sort, SortIdx: idx}
			}
			instance.Exports = exports

		default:
			return nil, offset, fmt.Errorf("instance[%d]: invalid kind %#x", i, kind)
		}

		instances[i] = instance
	}

	if bytesRead := offset - sectionStart; bytesRead > int(sectionSize) {
		return nil, offset, fmt.Errorf("instance section: read %d bytes but section is only %d", bytesRead, sectionSize)
	}
	return instances, offset, nil
}

// decodeAliasSection decodes the alias section (section 6).
// Section 6 contains vec(alias).
func decodeAliasSection(buf []byte, offset int, sectionSize uint32) ([]AliasDef, int, error) {
	sectionStart := offset

	count, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return nil, offset, fmt.Errorf("read count: %w", err)
	}
	offset += int(n)

	aliases := make([]AliasDef, count)
	for i := range count {
		if offset >= len(buf) {
			return nil, offset, ErrTruncatedBinary
		}
		sort := buf[offset]
		offset++

		var coreSort byte
		// Handle core sort (0x00 carries a discriminator: func/table/memory/
		// global/tag/type/module/instance) that the instance engine needs to
		// tell a core-func alias apart from a core-memory/table/global alias.
		if sort == 0x00 {
			if offset >= len(buf) {
				return nil, offset, ErrTruncatedBinary
			}
			coreSort = buf[offset]
			offset++
		}

		if offset >= len(buf) {
			return nil, offset, ErrTruncatedBinary
		}
		targetKind := buf[offset]
		offset++

		var alias AliasDef
		alias.Sort = sort
		alias.CoreSort = coreSort
		alias.TargetKind = targetKind

		switch targetKind {
		case 0x00, 0x01: // export or core export
			instanceIdx, n, err := leb128.LoadUint32(buf[offset:])
			if err != nil {
				return nil, offset, fmt.Errorf("alias[%d] instance index: %w", i, err)
			}
			offset += int(n)
			alias.InstanceIdx = instanceIdx

			// Alias names are simple labels, not extern names
			name, off, err := readLabel(buf, offset)
			if err != nil {
				return nil, off, fmt.Errorf("alias[%d] name: %w", i, err)
			}
			offset = off
			alias.Name = name

		case 0x02: // outer
			outerCount, n, err := leb128.LoadUint32(buf[offset:])
			if err != nil {
				return nil, offset, fmt.Errorf("alias[%d] outer count: %w", i, err)
			}
			offset += int(n)
			alias.OuterCount = outerCount

			outerIdx, n, err := leb128.LoadUint32(buf[offset:])
			if err != nil {
				return nil, offset, fmt.Errorf("alias[%d] outer index: %w", i, err)
			}
			offset += int(n)
			alias.OuterIndex = outerIdx

		default:
			return nil, offset, fmt.Errorf("alias[%d]: invalid target kind %#x", i, targetKind)
		}

		aliases[i] = alias
	}

	if bytesRead := offset - sectionStart; bytesRead > int(sectionSize) {
		return nil, offset, fmt.Errorf("alias section: read %d bytes but section is only %d", bytesRead, sectionSize)
	}
	return aliases, offset, nil
}

// decodeCanonSection decodes the canonical section (section 8).
// Section 8 contains vec(canon).
func decodeCanonSection(buf []byte, offset int, sectionSize uint32) ([]Canon, int, error) {
	sectionStart := offset

	count, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return nil, offset, fmt.Errorf("read count: %w", err)
	}
	offset += int(n)

	canons := make([]Canon, count)
	for i := range count {
		if offset >= len(buf) {
			return nil, offset, ErrTruncatedBinary
		}
		kind := buf[offset]
		offset++

		var canon Canon
		canon.Kind = kind

		switch kind {
		case CanonKindLift: // lift: 0x00 coreidx opts typeidx
			if offset >= len(buf) || buf[offset] != 0x00 {
				return nil, offset, fmt.Errorf("canon[%d]: expected 0x00 prefix for lift", i)
			}
			offset++

			coreFuncIdx, n, err := leb128.LoadUint32(buf[offset:])
			if err != nil {
				return nil, offset, fmt.Errorf("canon[%d] core func index: %w", i, err)
			}
			offset += int(n)
			canon.CoreFuncIdx = coreFuncIdx

			// Read opts: vec(canonopt)
			opts, off, err := decodeCanonOpts(buf, offset)
			if err != nil {
				return nil, off, fmt.Errorf("canon[%d] opts: %w", i, err)
			}
			offset = off
			canon.Opts = opts

			typeIdx, n, err := leb128.LoadUint32(buf[offset:])
			if err != nil {
				return nil, offset, fmt.Errorf("canon[%d] type index: %w", i, err)
			}
			offset += int(n)
			canon.TypeIdx = typeIdx

		case CanonKindLower: // lower: 0x01 funcidx opts
			if offset >= len(buf) || buf[offset] != 0x00 {
				return nil, offset, fmt.Errorf("canon[%d]: expected 0x00 prefix for lower", i)
			}
			offset++

			funcIdx, n, err := leb128.LoadUint32(buf[offset:])
			if err != nil {
				return nil, offset, fmt.Errorf("canon[%d] func index: %w", i, err)
			}
			offset += int(n)
			canon.FuncIdx = funcIdx

			// Read opts: vec(canonopt)
			opts, off, err := decodeCanonOpts(buf, offset)
			if err != nil {
				return nil, off, fmt.Errorf("canon[%d] opts: %w", i, err)
			}
			offset = off
			canon.Opts = opts

		case CanonKindResourceNew, CanonKindResourceDrop, CanonKindResourceRep, CanonKindResourceDropAsync: // typeidx
			typeIdx, n, err := leb128.LoadUint32(buf[offset:])
			if err != nil {
				return nil, offset, fmt.Errorf("canon[%d] type index: %w", i, err)
			}
			offset += int(n)
			canon.TypeIdx = typeIdx

		// --- Async builtins (Phase 0: decode + typing only, no execution) ---
		// Byte layouts verified against wasm-tools 1.253 `wasm-tools dump` on
		// internal/component/binary/testdata/async/*.wasm.

		case CanonKindTaskCancel, CanonKindSubtaskDrop,
			CanonKindWaitableSetNew, CanonKindWaitableSetDrop, CanonKindWaitableJoin,
			CanonKindBackpressureInc, CanonKindBackpressureDec,
			CanonKindErrorContextDrop:
			// No payload.

		case CanonKindSubtaskCancel: // subtask.cancel: async_:bool
			async, off, err := decodeBool(buf, offset)
			if err != nil {
				return nil, off, fmt.Errorf("canon[%d] async_: %w", i, err)
			}
			offset = off
			canon.Async = async

		case CanonKindTaskReturn: // task.return: resultlist opts
			// The result clause is encoded with the EXACT SAME grammar as a
			// functype's result list (readResultListDesc), not a bare
			// option<valtype> -- see Canon.TaskReturnResult's doc comment.
			result, off, err := readResultListDesc(buf, offset)
			if err != nil {
				return nil, off, fmt.Errorf("canon[%d] task.return result: %w", i, err)
			}
			offset = off
			canon.TaskReturnResult = result

			opts, off, err := decodeCanonOpts(buf, offset)
			if err != nil {
				return nil, off, fmt.Errorf("canon[%d] opts: %w", i, err)
			}
			offset = off
			canon.Opts = opts

		case CanonKindContextGet, CanonKindContextSet: // ty:core:valtype slot:u32
			if offset >= len(buf) {
				return nil, offset, ErrTruncatedBinary
			}
			canon.CoreValType = buf[offset]
			offset++

			slot, n, err := leb128.LoadUint32(buf[offset:])
			if err != nil {
				return nil, offset, fmt.Errorf("canon[%d] context slot: %w", i, err)
			}
			offset += int(n)
			canon.Slot = slot

		case CanonKindStreamNew, CanonKindFutureNew,
			CanonKindStreamDropReadable, CanonKindStreamDropWritable,
			CanonKindFutureDropReadable, CanonKindFutureDropWritable: // ty:typeidx
			typeIdx, n, err := leb128.LoadUint32(buf[offset:])
			if err != nil {
				return nil, offset, fmt.Errorf("canon[%d] type index: %w", i, err)
			}
			offset += int(n)
			canon.TypeIdx = typeIdx

		case CanonKindStreamRead, CanonKindStreamWrite,
			CanonKindFutureRead, CanonKindFutureWrite: // ty:typeidx opts
			typeIdx, n, err := leb128.LoadUint32(buf[offset:])
			if err != nil {
				return nil, offset, fmt.Errorf("canon[%d] type index: %w", i, err)
			}
			offset += int(n)
			canon.TypeIdx = typeIdx

			opts, off, err := decodeCanonOpts(buf, offset)
			if err != nil {
				return nil, off, fmt.Errorf("canon[%d] opts: %w", i, err)
			}
			offset = off
			canon.Opts = opts

		case CanonKindStreamCancelRead, CanonKindStreamCancelWrite,
			CanonKindFutureCancelRead, CanonKindFutureCancelWrite: // ty:typeidx async_:bool
			typeIdx, n, err := leb128.LoadUint32(buf[offset:])
			if err != nil {
				return nil, offset, fmt.Errorf("canon[%d] type index: %w", i, err)
			}
			offset += int(n)
			canon.TypeIdx = typeIdx

			async, off, err := decodeBool(buf, offset)
			if err != nil {
				return nil, off, fmt.Errorf("canon[%d] async_: %w", i, err)
			}
			offset = off
			canon.Async = async

		case CanonKindErrorContextNew, CanonKindErrorContextDebugMessage: // opts only
			opts, off, err := decodeCanonOpts(buf, offset)
			if err != nil {
				return nil, off, fmt.Errorf("canon[%d] opts: %w", i, err)
			}
			offset = off
			canon.Opts = opts

		case CanonKindWaitableSetWait, CanonKindWaitableSetPoll: // cancellable:bool memory:u32
			cancellable, off, err := decodeBool(buf, offset)
			if err != nil {
				return nil, off, fmt.Errorf("canon[%d] cancellable: %w", i, err)
			}
			offset = off
			canon.Cancellable = cancellable

			memIdx, n, err := leb128.LoadUint32(buf[offset:])
			if err != nil {
				return nil, offset, fmt.Errorf("canon[%d] memory index: %w", i, err)
			}
			offset += int(n)
			canon.MemIdx = memIdx

		// --- thread.* builtins (decode only -- see CanonKindThreadYield's
		// doc: no execution support exists; these five are decoded so a
		// component that merely DECLARES one, or traps before ever calling
		// it, doesn't fail loud at decode time for no reason). ---

		case CanonKindThreadIndex: // thread.index: no payload

		case CanonKindThreadNewIndirect: // thread.new-indirect: ft:typeidx tbl:core:tableidx
			ft, n, err := leb128.LoadUint32(buf[offset:])
			if err != nil {
				return nil, offset, fmt.Errorf("canon[%d] thread.new-indirect functype index: %w", i, err)
			}
			offset += int(n)
			canon.TypeIdx = ft

			tbl, n2, err := leb128.LoadUint32(buf[offset:])
			if err != nil {
				return nil, offset, fmt.Errorf("canon[%d] thread.new-indirect table index: %w", i, err)
			}
			offset += int(n2)
			canon.TableIdx = tbl

		case CanonKindThreadYield, CanonKindThreadSuspend, CanonKindThreadYieldThenResume: // cancel?:bool
			cancel, off, err := decodeBool(buf, offset)
			if err != nil {
				return nil, off, fmt.Errorf("canon[%d] cancel?: %w", i, err)
			}
			offset = off
			canon.Cancellable = cancel

		default:
			return nil, offset, fmt.Errorf("canon[%d]: async canon kind %#x not yet supported", i, kind)
		}

		canons[i] = canon
	}

	if bytesRead := offset - sectionStart; bytesRead > int(sectionSize) {
		return nil, offset, fmt.Errorf("canon section: read %d bytes but section is only %d", bytesRead, sectionSize)
	}
	return canons, offset, nil
}

// decodeBool reads a single bool byte (0x00 = false, 0x01 = true), used by
// several async canon payloads (subtask.cancel's async_, waitable-set.wait/
// poll's cancellable, stream/future cancel-read/-write's async_).
func decodeBool(buf []byte, offset int) (bool, int, error) {
	if offset >= len(buf) {
		return false, offset, ErrTruncatedBinary
	}
	b := buf[offset]
	offset++
	switch b {
	case 0x00:
		return false, offset, nil
	case 0x01:
		return true, offset, nil
	default:
		return false, offset, fmt.Errorf("invalid bool byte %#x", b)
	}
}

// decodeCanonOpts decodes vec(canonopt).
func decodeCanonOpts(buf []byte, offset int) ([]CanonOpt, int, error) {
	count, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return nil, offset, fmt.Errorf("read count: %w", err)
	}
	offset += int(n)

	opts := make([]CanonOpt, count)
	for i := range count {
		if offset >= len(buf) {
			return nil, offset, ErrTruncatedBinary
		}
		optKind := buf[offset]
		offset++

		var opt CanonOpt
		opt.Kind = optKind

		switch optKind {
		case 0x00, 0x01, 0x02: // string encodings: no data
			// utf8, utf16, latin1+utf16
		case 0x03, 0x04, 0x05, 0x07: // memory, realloc, post-return, callback: carry an index
			idx, n, err := leb128.LoadUint32(buf[offset:])
			if err != nil {
				return nil, offset, fmt.Errorf("canonopt[%d] index: %w", i, err)
			}
			offset += int(n)
			opt.Idx = idx
		case 0x06: // async: no data
		default:
			return nil, offset, fmt.Errorf("canonopt[%d]: unsupported (M1) kind %#x", i, optKind)
		}

		opts[i] = opt
	}

	return opts, offset, nil
}

// decodeStartSection decodes the start section (section 9).
// Section 9 contains: funcidx vec(valueidx) resultcount.
func decodeStartSection(buf []byte, offset int, sectionSize uint32) (*Start, int, error) {
	sectionStart := offset

	funcIdx, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return nil, offset, fmt.Errorf("func index: %w", err)
	}
	offset += int(n)

	// Read args: vec(valueidx)
	argCount, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return nil, offset, fmt.Errorf("arg count: %w", err)
	}
	offset += int(n)

	args := make([]uint32, argCount)
	for i := range argCount {
		arg, n, err := leb128.LoadUint32(buf[offset:])
		if err != nil {
			return nil, offset, fmt.Errorf("arg[%d]: %w", i, err)
		}
		offset += int(n)
		args[i] = arg
	}

	// Read result count
	resultCount, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return nil, offset, fmt.Errorf("result count: %w", err)
	}
	offset += int(n)

	start := &Start{
		FuncIdx:     funcIdx,
		Args:        args,
		ResultCount: resultCount,
	}

	if bytesRead := offset - sectionStart; bytesRead > int(sectionSize) {
		return nil, offset, fmt.Errorf("start section: read %d bytes but section is only %d", bytesRead, sectionSize)
	}
	return start, offset, nil
}
