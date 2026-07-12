package binary

import (
	"fmt"
	"io"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/leb128"
	"github.com/samyfodil/wazy/internal/wasm"
)

func decodeTypeSection(enabledFeatures api.CoreFeatures, buf []byte, offset int) ([]wasm.FunctionType, int, error) {
	vs, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return nil, offset, fmt.Errorf("get size of vector: %w", err)
	}
	offset += int(n)

	// vtArena batches the Params/Results slices of every function type in this section into a few chunks.
	var vtArena valueTypeArena
	var result []wasm.FunctionType
	for i := uint32(0); i < vs; i++ {
		// Peek at the leading byte to check for rec group (0x4e, GC proposal).
		b, o, err := readByte(buf, offset)
		if err != nil {
			return nil, offset, fmt.Errorf("read %d-th type: %v", i, err)
		}
		if b == 0x4e {
			// Rec group: contains multiple types.
			offset = o
			recCount, n, err := leb128.LoadUint32(buf[offset:])
			if err != nil {
				return nil, offset, fmt.Errorf("read rec group count: %v", err)
			}
			offset += int(n)
			startIdx := uint32(len(result))
			for j := uint32(0); j < recCount; j++ {
				var ft wasm.FunctionType
				if offset, err = decodeFunctionType(enabledFeatures, buf, offset, &vtArena, &ft); err != nil {
					return nil, offset, fmt.Errorf("read %d-th type in rec group: %v", j, err)
				}
				ft.RecGroupSize = int(recCount)
				ft.RecGroupPosition = int(j)
				result = append(result, ft)
			}
			for j := uint32(0); j < recCount; j++ {
				if err := validateTypeForwardRefs(&result[startIdx+j], startIdx+recCount); err != nil {
					return nil, offset, err
				}
			}
		} else {
			// Decode as a regular function type starting from the same offset: since we never advanced past
			// the peeked byte, there's nothing to "put back" the way the reader-based code needed to.
			var ft wasm.FunctionType
			if offset, err = decodeFunctionType(enabledFeatures, buf, offset, &vtArena, &ft); err != nil {
				return nil, offset, fmt.Errorf("read %d-th type: %v", i, err)
			}
			if err := validateTypeForwardRefs(&ft, uint32(len(result))); err != nil {
				return nil, offset, err
			}
			result = append(result, ft)
		}
	}
	return result, offset, nil
}

// validateTypeForwardRefs rejects concrete reference types (ref $t) whose type
// index is not yet defined. For standalone types, maxTypeIndex is the count of
// types decoded so far; for rec groups, it is the index after the last member,
// allowing mutual references within the group.
func validateTypeForwardRefs(ft *wasm.FunctionType, maxTypeIndex uint32) error {
	for i, vt := range ft.Params {
		if vt.IsConcreteRef() && vt.TypeIndex() >= maxTypeIndex {
			return fmt.Errorf("unknown type index %d in param[%d]", vt.TypeIndex(), i)
		}
	}
	for i, vt := range ft.Results {
		if vt.IsConcreteRef() && vt.TypeIndex() >= maxTypeIndex {
			return fmt.Errorf("unknown type index %d in result[%d]", vt.TypeIndex(), i)
		}
	}
	return nil
}

// decodeImportSection decodes the decoded import segments plus the count per wasm.ExternType.
func decodeImportSection(
	buf []byte,
	offset int,
	arena *stringArena,
	memorySizer memorySizer,
	memoryLimitPages uint32,
	enabledFeatures api.CoreFeatures,
) (result []wasm.Import,
	perModule map[string][]*wasm.Import,
	funcCount, globalCount, memoryCount, tableCount, tagCount wasm.Index,
	newOffset int, err error,
) {
	vs, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		err = fmt.Errorf("get size of vector: %w", err)
		return nil, nil, 0, 0, 0, 0, 0, offset, err
	}
	offset += int(n)

	perModule = make(map[string][]*wasm.Import)
	result = make([]wasm.Import, vs)
	var prevModule string
	for i := uint32(0); i < vs; i++ {
		imp := &result[i]
		if offset, err = decodeImport(buf, offset, i, memorySizer, memoryLimitPages, enabledFeatures, arena, prevModule, imp); err != nil {
			return nil, nil, 0, 0, 0, 0, 0, offset, err
		}
		prevModule = imp.Module
		switch imp.Type {
		case wasm.ExternTypeFunc:
			imp.IndexPerType = funcCount
			funcCount++
		case wasm.ExternTypeGlobal:
			imp.IndexPerType = globalCount
			globalCount++
		case wasm.ExternTypeMemory:
			imp.IndexPerType = memoryCount
			memoryCount++
		case wasm.ExternTypeTable:
			imp.IndexPerType = tableCount
			tableCount++
		case wasm.ExternTypeTag:
			imp.IndexPerType = tagCount
			tagCount++
		}
		perModule[imp.Module] = append(perModule[imp.Module], imp)
	}
	return result, perModule, funcCount, globalCount, memoryCount, tableCount, tagCount, offset, nil
}

func decodeFunctionSection(buf []byte, offset int) ([]uint32, int, error) {
	vs, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return nil, offset, fmt.Errorf("get size of vector: %w", err)
	}
	offset += int(n)

	result := make([]uint32, vs)
	for i := uint32(0); i < vs; i++ {
		v, n, err := leb128.LoadUint32(buf[offset:])
		if err != nil {
			return nil, offset, fmt.Errorf("get type index: %w", err)
		}
		result[i] = v
		offset += int(n)
	}
	return result, offset, nil
}

func decodeTableSection(buf []byte, offset int, enabledFeatures api.CoreFeatures) ([]wasm.Table, int, error) {
	vs, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return nil, offset, fmt.Errorf("error reading size")
	}
	offset += int(n)
	if vs > 1 {
		if err := enabledFeatures.RequireEnabled(api.CoreFeatureReferenceTypes); err != nil {
			return nil, offset, fmt.Errorf("at most one table allowed in module as %w", err)
		}
	}

	ret := make([]wasm.Table, vs)
	for i := range ret {
		offset, err = decodeTable(buf, offset, enabledFeatures, &ret[i])
		if err != nil {
			return nil, offset, err
		}
	}
	return ret, offset, nil
}

func decodeMemorySection(
	buf []byte,
	offset int,
	enabledFeatures api.CoreFeatures,
	memorySizer memorySizer,
	memoryLimitPages uint32,
) (*wasm.Memory, int, error) {
	vs, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return nil, offset, fmt.Errorf("error reading size")
	}
	offset += int(n)
	if vs > 1 {
		return nil, offset, fmt.Errorf("at most one memory allowed in module, but read %d", vs)
	} else if vs == 0 {
		// memory count can be zero.
		return nil, offset, nil
	}

	return decodeMemory(buf, offset, enabledFeatures, memorySizer, memoryLimitPages)
}

func decodeGlobalSection(buf []byte, offset int, enabledFeatures api.CoreFeatures) ([]wasm.Global, int, error) {
	vs, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return nil, offset, fmt.Errorf("get size of vector: %w", err)
	}
	offset += int(n)

	result := make([]wasm.Global, vs)
	for i := uint32(0); i < vs; i++ {
		if offset, err = decodeGlobal(buf, offset, enabledFeatures, &result[i]); err != nil {
			return nil, offset, fmt.Errorf("global[%d]: %w", i, err)
		}
	}
	return result, offset, nil
}

func decodeExportSection(buf []byte, offset int, arena *stringArena) ([]wasm.Export, map[string]*wasm.Export, int, error) {
	vs, n, sizeErr := leb128.LoadUint32(buf[offset:])
	if sizeErr != nil {
		return nil, nil, offset, fmt.Errorf("get size of vector: %v", sizeErr)
	}
	offset += int(n)

	exportMap := make(map[string]*wasm.Export, vs)
	exportSection := make([]wasm.Export, vs)
	for i := wasm.Index(0); i < vs; i++ {
		export := &exportSection[i]
		var err error
		offset, err = decodeExport(buf, offset, arena, export)
		if err != nil {
			return nil, nil, offset, fmt.Errorf("read export: %w", err)
		}
		if _, ok := exportMap[export.Name]; ok {
			return nil, nil, offset, fmt.Errorf("export[%d] duplicates name %q", i, export.Name)
		} else {
			exportMap[export.Name] = export
		}
	}
	return exportSection, exportMap, offset, nil
}

func decodeStartSection(buf []byte, offset int) (*wasm.Index, int, error) {
	vs, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return nil, offset, fmt.Errorf("get function index: %w", err)
	}
	offset += int(n)
	return &vs, offset, nil
}

func decodeElementSection(buf []byte, offset int, enabledFeatures api.CoreFeatures) ([]wasm.ElementSegment, int, error) {
	vs, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return nil, offset, fmt.Errorf("get size of vector: %w", err)
	}
	offset += int(n)

	result := make([]wasm.ElementSegment, vs)
	for i := uint32(0); i < vs; i++ {
		if offset, err = decodeElementSegment(buf, offset, enabledFeatures, &result[i]); err != nil {
			return nil, offset, fmt.Errorf("read element: %w", err)
		}
	}
	return result, offset, nil
}

func decodeCodeSection(buf []byte, offset int, sectionSize uint32) ([]wasm.Code, int, error) {
	codeSectionStart := offset
	vs, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return nil, offset, fmt.Errorf("get size of vector: %w", err)
	}
	offset += int(n)

	result := make([]wasm.Code, vs)
	// One backing array holds every function body in this section. Bodies are copied into sub-slices of it and
	// retained on wasm.Code.Body for the module's lifetime, which is identical retention to the previous N
	// separate make([]byte)+copy per body — just one allocation instead of N. The section size is a safe upper
	// bound (bodies are a strict subset of the section's bytes; size headers and locals live in the same span).
	arena := make([]byte, sectionSize)
	arenaOff := 0
	// localTypes batches every function's Code.LocalTypes into a few chunks instead of one slice per function.
	var localTypes valueTypeArena
	// locals is a scratch buffer reused across every function in the section so the single-pass locals decode
	// (see decodeCode) allocates at most once per section rather than re-deriving groups per function.
	var locals []localsGroup
	for i := uint32(0); i < vs; i++ {
		offset, arenaOff, err = decodeCode(buf, offset, codeSectionStart, arena, arenaOff, &localTypes, &locals, &result[i])
		if err != nil {
			return nil, offset, fmt.Errorf("read %d-th code segment: %v", i, err)
		}
	}
	return result, offset, nil
}

func decodeDataSection(buf []byte, offset int, enabledFeatures api.CoreFeatures) ([]wasm.DataSegment, int, error) {
	vs, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return nil, offset, fmt.Errorf("get size of vector: %w", err)
	}
	offset += int(n)

	result := make([]wasm.DataSegment, vs)
	for i := uint32(0); i < vs; i++ {
		if offset, err = decodeDataSegment(buf, offset, enabledFeatures, &result[i]); err != nil {
			return nil, offset, fmt.Errorf("read data segment: %w", err)
		}
	}
	return result, offset, nil
}

func decodeTagSection(buf []byte, offset int) ([]wasm.Tag, int, error) {
	vs, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return nil, offset, fmt.Errorf("get size of vector: %w", err)
	}
	offset += int(n)

	result := make([]wasm.Tag, vs)
	for i := uint32(0); i < vs; i++ {
		// Read attribute byte (must be 0x00 per spec).
		attr, o, err := readByte(buf, offset)
		if err != nil {
			return nil, offset, fmt.Errorf("read tag[%d] attribute: %w", i, err)
		}
		offset = o
		if attr != 0x00 {
			return nil, offset, fmt.Errorf("tag[%d] has invalid attribute: %#x", i, attr)
		}
		// Read type index.
		var tn uint64
		result[i].Type, tn, err = leb128.LoadUint32(buf[offset:])
		if err != nil {
			return nil, offset, fmt.Errorf("read tag[%d] type index: %w", i, err)
		}
		offset += int(tn)
	}
	return result, offset, nil
}

func decodeDataCountSection(buf []byte, offset int) (count *uint32, newOffset int, err error) {
	v, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil && err != io.EOF {
		// data count is optional, so EOF is fine.
		return nil, offset, err
	}
	if err == nil {
		offset += int(n)
	}
	return &v, offset, nil
}
