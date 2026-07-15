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
			// Skip custom sections entirely for M1.
			offset += int(sectionSize)

		case 7: // Type section
			types, newOffset, err := decodeTypeSection(buf, offset, sectionSize)
			if err != nil {
				return nil, fmt.Errorf("type section: %w", err)
			}
			offset = newOffset
			base := uint32(len(c.Types))
			for j := range types {
				types[j].Index = base + uint32(j)
			}
			c.Types = append(c.Types, types...)

		case 10: // Import section
			imports, newOffset, err := decodeImportSection(buf, offset, sectionSize)
			if err != nil {
				return nil, fmt.Errorf("import section: %w", err)
			}
			offset = newOffset
			c.Imports = append(c.Imports, imports...)

		case 11: // Export section
			exports, newOffset, err := decodeExportSection(buf, offset, sectionSize)
			if err != nil {
				return nil, fmt.Errorf("export section: %w", err)
			}
			offset = newOffset
			c.Exports = append(c.Exports, exports...)

		default:
			// Record and skip unknown/unimplemented sections.
			c.RawSections = append(c.RawSections, RawSection{ID: sectionID, Size: sectionSize})
			offset += int(sectionSize)
		}

		// Verify we consumed exactly the right number of bytes.
		bytesRead := offset - sectionStart
		if int(sectionSize) != bytesRead {
			return nil, fmt.Errorf("section %s: expected %d bytes but read %d", sectionIDName(sectionID), sectionSize, bytesRead)
		}
	}

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
		sort, off2, err := readExterndesc(buf, off)
		if err != nil {
			return nil, off2, fmt.Errorf("import[%d] externdesc: %w", i, err)
		}
		offset = off2
		imports[i] = Import{Name: name, ExternType: sort}
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
			if _, off2, err = readExterndesc(buf, off2); err != nil {
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
