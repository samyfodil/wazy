package binary

import (
	"fmt"
	"io"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/leb128"
	"github.com/samyfodil/wazy/internal/wasm"
)

func decodeTypeSection(enabledFeatures api.CoreFeatures, buf []byte, offset int, sectionSize uint32) ([]wasm.FunctionType, int, error) {
	vs, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return nil, offset, fmt.Errorf("get size of vector: %w", err)
	}
	offset += int(n)

	// vtArena batches the Params/Results slices of every function type in this section into a few chunks.
	var vtArena valueTypeArena
	// Size from the section's own count, clamped to what this section's bytes could possibly encode (the
	// shortest type entry is 2 bytes), so an inflated count can't drive a huge allocation. Both the count and
	// the size come from the untrusted module, hence the clamp against the bytes actually present too.
	result := make([]wasm.FunctionType, 0, min(uint64(vs), uint64(arenaSize(sectionSize, len(buf)-offset))/2))
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
				if offset, err = decodeDefinedType(enabledFeatures, buf, offset, &vtArena, &ft); err != nil {
					return nil, offset, fmt.Errorf("read %d-th type in rec group: %v", j, err)
				}
				ft.RecGroupSize = int(recCount)
				ft.RecGroupPosition = int(j)
				// Cache the key only now that the rec group fields are set, since they are part of it. Decoding
				// is single-threaded and this type is not yet shared, whereas the runtime call_indirect helpers
				// reach key() through Store.GetFunctionTypeID on a *shared* FunctionType -- e.g.
				// internal/emscripten (*InvokeFunc).Call and table.LookupFunction both call it while executing
				// guest code, which is multi-goroutine -- so key() itself never writes.
				ft.CacheKey()
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
			if offset, err = decodeDefinedType(enabledFeatures, buf, offset, &vtArena, &ft); err != nil {
				return nil, offset, fmt.Errorf("read %d-th type: %v", i, err)
			}
			// Under GC a standalone type is its own rec group of one, so it may reference itself: hence
			// +1. Typed function references alone does not have implicit rec groups, and rejects it.
			selfRef := uint32(0)
			if enabledFeatures.IsEnabled(api.CoreFeatureGC) {
				selfRef = 1
			}
			if err := validateTypeForwardRefs(&ft, uint32(len(result))+selfRef); err != nil {
				return nil, offset, err
			}
			ft.CacheKey()
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
	for i, f := range ft.Fields {
		if f.Type.IsConcreteRef() && f.Type.TypeIndex() >= maxTypeIndex {
			return fmt.Errorf("unknown type index %d in field[%d]", f.Type.TypeIndex(), i)
		}
	}
	if ft.HasSupertype && ft.Supertype >= maxTypeIndex {
		return fmt.Errorf("unknown supertype index %d", ft.Supertype)
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
	}

	// ImportPerModule's slices are runs of one backing array rather than N slices each grown from nil:
	// imports are grouped by module in practice, so a namespace is almost always one contiguous run. A
	// namespace interrupted by another and then resumed appends onto its earlier run, which reallocates
	// (the runs are capacity-capped) and so cannot clobber the following one.
	backing := make([]*wasm.Import, vs)
	for i := range result {
		backing[i] = &result[i]
	}
	for start := 0; start < len(backing); {
		module := backing[start].Module
		end := start + 1
		for end < len(backing) && backing[end].Module == module {
			end++
		}
		if existing, ok := perModule[module]; ok {
			perModule[module] = append(existing, backing[start:end:end]...)
		} else {
			perModule[module] = backing[start:end:end]
		}
		start = end
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
) ([]wasm.Memory, int, error) {
	vs, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return nil, offset, fmt.Errorf("error reading size")
	}
	offset += int(n)
	if vs > 1 {
		if err := enabledFeatures.RequireEnabled(api.CoreFeatureMultiMemory); err != nil {
			return nil, offset, fmt.Errorf("at most one memory allowed in module as %w", err)
		}
	}
	// Each memory entry is at least 2 bytes (a limits-flags byte plus a
	// 1-byte-minimum LEB128 min), so an attacker-controlled vs bigger than
	// half the remaining buffer can never be satisfied. Reject it before the
	// allocation below, rather than after requesting up to vs*sizeof(Memory)
	// bytes for a module that can't possibly contain that many entries.
	// Compared as uint64 throughout: remaining, as an int, can exceed 4GiB on
	// a 64-bit platform, and truncating it to uint32 first could wrap and
	// falsely reject a legitimately large module.
	remaining := uint64(len(buf) - offset)
	if uint64(vs) > remaining/2 {
		return nil, offset, fmt.Errorf("memory section size %d exceeds remaining module bytes (%d)", vs, remaining)
	}
	if vs > wasm.MaximumMemoryIndex {
		return nil, offset, fmt.Errorf("memory section size %d exceeds the limit of %d", vs, wasm.MaximumMemoryIndex)
	}

	ret := make([]wasm.Memory, vs)
	// Each memory is individually bounded by memoryLimitPages -- the
	// embedder's own configured choice, e.g. via WithMemoryLimitPages -- but
	// nothing otherwise stops a multi-memory module from eagerly demanding N
	// times that much once buildMemory allocates every declared memory's
	// minimum. Bound the SUM of each memory's Min (not Cap) to that same
	// embedder-configured ceiling: Min bytes are unconditionally touched at
	// instantiation regardless of allocator or config (NewMemoryInstance's
	// minBytes slice/backing, or a custom allocator's Reallocate(minBytes)),
	// so it's the metric that's actually always "eager". Cap, by contrast, is
	// config-dependent -- WithMemoryCapacityFromMax inflates every max-less
	// memory's Cap to the full memoryLimitPages, which made a Cap-based sum
	// reject ordinary two-memory modules outright under that (documented,
	// public) config, even though nothing was eagerly over-allocated by the
	// module itself. Min is attacker-controlled (part of the untrusted wasm
	// binary); Cap-beyond-Min is an embedder-controlled performance knob the
	// embedder already opted into with informed consent (WithMemoryCapacityFromMax's
	// own doc warns about its per-memory cost) -- this check exists to bound
	// what the module can demand, not to second-guess the embedder's own
	// tuning.
	//
	// A 64-bit memory is deliberately left out of the sum: the specification
	// caps its declared minimum at 2^48 pages, not at anything an embedder
	// configured, and requires a module declaring that much to still be valid
	// (test/core/memory64.wast defines exactly such a module without
	// instantiating it). Rejecting it here would reject a conformant module,
	// so the same eager-allocation bound is applied to 64-bit memories at
	// instantiation instead -- see ModuleInstance.buildMemory, which is also
	// where nothing has been allocated yet.
	var totalMinPages uint64
	for i := range ret {
		var mem *wasm.Memory
		mem, offset, err = decodeMemory(buf, offset, enabledFeatures, memorySizer, memoryLimitPages)
		if err != nil {
			return nil, offset, err
		}
		ret[i] = *mem
		if mem.IsMemory64 {
			continue
		}
		totalMinPages += mem.Min
		if totalMinPages > uint64(memoryLimitPages) {
			return nil, offset, fmt.Errorf("total memory minimum across %d memories (%d pages) exceeds %d pages",
				i+1, totalMinPages, memoryLimitPages)
		}
	}
	return ret, offset, nil
}

func decodeGlobalSection(buf []byte, offset int, enabledFeatures api.CoreFeatures) ([]wasm.Global, int, error) {
	vs, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return nil, offset, fmt.Errorf("get size of vector: %w", err)
	}
	offset += int(n)

	result := make([]wasm.Global, vs)
	// One arena for every global's init expression, as the code section does for bodies.
	var ba byteArena
	for i := uint32(0); i < vs; i++ {
		if offset, err = decodeGlobal(buf, offset, enabledFeatures, &ba, &result[i]); err != nil {
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
	// One arena for every segment's offset expression.
	var ba byteArena
	for i := uint32(0); i < vs; i++ {
		if offset, err = decodeElementSegment(buf, offset, enabledFeatures, &ba, &result[i]); err != nil {
			return nil, offset, fmt.Errorf("read element: %w", err)
		}
	}
	return result, offset, nil
}

func decodeCodeSection(buf []byte, offset int, sectionSize uint32, enabledFeatures api.CoreFeatures) ([]wasm.Code, int, error) {
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
	arena := make([]byte, arenaSize(sectionSize, len(buf)-offset))
	arenaOff := 0
	// localTypes batches every function's Code.LocalTypes into a few chunks instead of one slice per function.
	var localTypes valueTypeArena
	// locals is a scratch buffer reused across every function in the section so the single-pass locals decode
	// (see decodeCode) allocates at most once per section rather than re-deriving groups per function.
	var locals []localsGroup
	for i := uint32(0); i < vs; i++ {
		offset, arenaOff, err = decodeCode(buf, offset, codeSectionStart, arena, arenaOff, &localTypes, &locals, enabledFeatures, &result[i])
		if err != nil {
			return nil, offset, fmt.Errorf("read %d-th code segment: %v", i, err)
		}
	}
	return result, offset, nil
}

func decodeDataSection(buf []byte, offset int, sectionSize uint32, enabledFeatures api.CoreFeatures) ([]wasm.DataSegment, int, error) {
	vs, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return nil, offset, fmt.Errorf("get size of vector: %w", err)
	}
	offset += int(n)

	result := make([]wasm.DataSegment, vs)
	// One backing array holds every segment's Init plus its offset expression, exactly as the code section
	// does for bodies: both are copies of disjoint spans of this section, so the section size is a safe upper
	// bound for their sum and the whole section needs a single allocation. It is clamped to the bytes actually
	// remaining, since sectionSize comes from the module's own (untrusted) header.
	ba := byteArena{buf: make([]byte, 0, arenaSize(sectionSize, len(buf)-offset))}
	for i := uint32(0); i < vs; i++ {
		if offset, err = decodeDataSegment(buf, offset, enabledFeatures, &ba, &result[i]); err != nil {
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
