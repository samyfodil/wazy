package binary

import (
	"bytes"
	"debug/dwarf"
	"errors"
	"fmt"
	"io"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/experimental"
	"github.com/samyfodil/wazy/internal/leb128"
	"github.com/samyfodil/wazy/internal/wasm"
	"github.com/samyfodil/wazy/internal/wasmdebug"
)

// DecodeModule implements wasm.DecodeModule for the WebAssembly 1.0 (20191205) Binary Format
// See https://www.w3.org/TR/2019/REC-wasm-core-1-20191205/#binary-format%E2%91%A0
func DecodeModule(
	binary []byte,
	enabledFeatures api.CoreFeatures,
	memoryLimitPages uint32,
	memoryCapacityFromMax,
	dwarfEnabled, storeCustomSections bool,
) (*wasm.Module, error) {
	// Magic number.
	if len(binary) < 4 || !bytes.Equal(binary[:4], Magic) {
		return nil, ErrInvalidMagicNumber
	}

	// Version.
	if len(binary) < 8 || !bytes.Equal(binary[4:8], version) {
		return nil, ErrInvalidVersion
	}

	memSizer := newMemorySizer(memoryLimitPages, memoryCapacityFromMax)

	m := &wasm.Module{}
	// arena batches every string decoded for this module (import/export/custom/name-section strings) into a few
	// []byte chunks instead of one allocation per string. It lives only for this DecodeModule call, so concurrent
	// DecodeModule calls never share it.
	arena := &stringArena{}
	var lastSectionID wasm.SectionID
	var info, line, str, abbrev, ranges []byte // For DWARF Data.
	offset := 8
	for offset < len(binary) {
		// TODO: except custom sections, all others are required to be in order, but we aren't checking yet.
		// See https://www.w3.org/TR/2019/REC-wasm-core-1-20191205/#modules%E2%91%A0%E2%93%AA
		sectionID := binary[offset]
		offset++

		sectionSize, n, err := leb128.LoadUint32(binary[offset:])
		if err != nil {
			return nil, fmt.Errorf("get size of section %s: %v", wasm.SectionIDName(sectionID), err)
		}
		offset += int(n)

		var ok bool
		lastSectionID, ok = checkSectionOrder(sectionID, lastSectionID)
		if !ok {
			return nil, errors.New("invalid section order")
		}

		sectionContentStart := offset
		switch sectionID {
		case wasm.SectionIDCustom:
			// First, validate the section and determine if the section for this name has already been set
			name, nameEndOffset, decodeErr := decodeUTF8(binary, offset, arena, "custom section name")
			if decodeErr != nil {
				err = decodeErr
				break
			}
			nameSize := uint32(nameEndOffset - offset)
			offset = nameEndOffset
			if sectionSize < nameSize {
				err = fmt.Errorf("malformed custom section %s", name)
				break
			} else if name == "name" && m.NameSection != nil {
				err = fmt.Errorf("redundant custom section %s", name)
				break
			}

			// Now, either decode the NameSection or CustomSection
			limit := sectionSize - nameSize

			var c *wasm.CustomSection
			if name != "name" {
				if storeCustomSections || dwarfEnabled {
					c, offset, err = decodeCustomSection(binary, offset, name, uint64(limit))
					if err != nil {
						return nil, fmt.Errorf("failed to read custom section name[%s]: %w", name, err)
					}
					m.CustomSections = append(m.CustomSections, c)
					if dwarfEnabled {
						switch name {
						case ".debug_info":
							info = c.Data
						case ".debug_line":
							line = c.Data
						case ".debug_str":
							str = c.Data
						case ".debug_abbrev":
							abbrev = c.Data
						case ".debug_ranges":
							ranges = c.Data
						}
					}
				} else {
					// Mirrors io.CopyN's behavior (used here prior to the slice-indexed rewrite): io.EOF
					// whenever fewer than limit bytes remain, never io.ErrUnexpectedEOF for a partial remainder.
					if int(limit) > len(binary)-offset {
						return nil, fmt.Errorf("failed to skip name[%s]: %w", name, io.EOF)
					}
					offset += int(limit)
				}
			} else {
				m.NameSection, offset, err = decodeNameSection(binary, offset, arena, uint64(limit))
			}
		case wasm.SectionIDType:
			m.TypeSection, offset, err = decodeTypeSection(enabledFeatures, binary, offset)
		case wasm.SectionIDImport:
			m.ImportSection, m.ImportPerModule, m.ImportFunctionCount, m.ImportGlobalCount, m.ImportMemoryCount, m.ImportTableCount, m.ImportTagCount, offset, err = decodeImportSection(binary, offset, arena, memSizer, memoryLimitPages, enabledFeatures)
			if err != nil {
				return nil, err // avoid re-wrapping the error.
			}
		case wasm.SectionIDFunction:
			m.FunctionSection, offset, err = decodeFunctionSection(binary, offset)
		case wasm.SectionIDTable:
			m.TableSection, offset, err = decodeTableSection(binary, offset, enabledFeatures)
		case wasm.SectionIDMemory:
			m.MemorySection, offset, err = decodeMemorySection(binary, offset, enabledFeatures, memSizer, memoryLimitPages)
		case wasm.SectionIDTag:
			if err := enabledFeatures.RequireEnabled(experimental.CoreFeaturesExceptionHandling); err != nil {
				return nil, fmt.Errorf("tag section not supported as %v", err)
			}
			m.TagSection, offset, err = decodeTagSection(binary, offset)
		case wasm.SectionIDGlobal:
			if m.GlobalSection, offset, err = decodeGlobalSection(binary, offset, enabledFeatures); err != nil {
				return nil, err // avoid re-wrapping the error.
			}
		case wasm.SectionIDExport:
			m.ExportSection, m.Exports, offset, err = decodeExportSection(binary, offset, arena)
		case wasm.SectionIDStart:
			m.StartSection, offset, err = decodeStartSection(binary, offset)
		case wasm.SectionIDElement:
			m.ElementSection, offset, err = decodeElementSection(binary, offset, enabledFeatures)
		case wasm.SectionIDCode:
			m.CodeSection, offset, err = decodeCodeSection(binary, offset, sectionSize)
		case wasm.SectionIDData:
			m.DataSection, offset, err = decodeDataSection(binary, offset, enabledFeatures)
		case wasm.SectionIDDataCount:
			if err := enabledFeatures.RequireEnabled(api.CoreFeatureBulkMemoryOperations); err != nil {
				return nil, fmt.Errorf("data count section not supported as %v", err)
			}
			m.DataCountSection, offset, err = decodeDataCountSection(binary, offset)
		default:
			err = ErrInvalidSectionID
		}

		readBytes := offset - sectionContentStart
		if err == nil && int(sectionSize) != readBytes {
			err = fmt.Errorf("invalid section length: expected to be %d but got %d", sectionSize, readBytes)
		}

		if err != nil {
			return nil, fmt.Errorf("section %s: %v", wasm.SectionIDName(sectionID), err)
		}
	}

	if dwarfEnabled {
		d, _ := dwarf.New(abbrev, nil, nil, info, line, nil, ranges, str)
		m.DWARFLines = wasmdebug.NewDWARFLines(d)
	}

	functionCount, codeCount := m.SectionElementCount(wasm.SectionIDFunction), m.SectionElementCount(wasm.SectionIDCode)
	if functionCount != codeCount {
		return nil, fmt.Errorf("function and code section have inconsistent lengths: %d != %d", functionCount, codeCount)
	}
	return m, nil
}

func checkSectionOrder(current, previous wasm.SectionID) (byte, bool) {
	// https://webassembly.github.io/spec/core/binary/modules.html#binary-module

	// Custom sections can show up anywhere.
	if current == wasm.SectionIDCustom {
		return previous, true
	}

	// Tag section (ID 13) must come after Memory (5) and before Global (6).
	if current == wasm.SectionIDTag {
		return current, previous <= wasm.SectionIDMemory
	}
	if previous == wasm.SectionIDTag {
		return current, current >= wasm.SectionIDGlobal
	}

	// DataCount was introduced in Wasm 2.0.
	// It must come after Element and before Code.
	if current > wasm.SectionIDDataCount {
		return current, false
	}
	if current == wasm.SectionIDDataCount {
		return current, previous <= wasm.SectionIDElement
	}
	if previous == wasm.SectionIDDataCount {
		return current, current >= wasm.SectionIDCode
	}

	// Otherwise, strictly increasing order.
	return current, current > previous
}

// memorySizer derives min, capacity and max pages from decoded wasm.
type memorySizer func(minPages uint32, maxPages *uint32) (min uint32, capacity uint32, max uint32)

// newMemorySizer sets capacity to minPages unless max is defined and
// memoryCapacityFromMax is true.
func newMemorySizer(memoryLimitPages uint32, memoryCapacityFromMax bool) memorySizer {
	return func(minPages uint32, maxPages *uint32) (min, capacity, max uint32) {
		if maxPages != nil {
			if memoryCapacityFromMax {
				return minPages, *maxPages, *maxPages
			}
			// This is an invalid value: let it propagate, we will fail later.
			if *maxPages > wasm.MemoryLimitPages {
				return minPages, minPages, *maxPages
			}
			// This is a valid value, but it goes over the run-time limit: return the limit.
			if *maxPages > memoryLimitPages {
				return minPages, minPages, memoryLimitPages
			}
			return minPages, minPages, *maxPages
		}
		if memoryCapacityFromMax {
			return minPages, memoryLimitPages, memoryLimitPages
		}
		return minPages, minPages, memoryLimitPages
	}
}
