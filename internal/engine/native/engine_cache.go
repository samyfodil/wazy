package native

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"unsafe"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
	"github.com/samyfodil/wazy/internal/filecache"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/u32"
	"github.com/samyfodil/wazy/internal/u64"
	"github.com/samyfodil/wazy/internal/wasm"
)

var crc = crc32.MakeTable(crc32.Castagnoli)

// fileCacheKey returns a key for the file cache.
// In order to avoid collisions with the existing compiler, we do not use m.ID directly,
// but instead we rehash it with magic.
func fileCacheKey(m *wasm.Module) (ret filecache.Key) {
	s := sha256.New()
	s.Write(m.ID[:])
	s.Write(magic)
	// Write the CPU features so that we can cache the compiled module for the same CPU.
	// This prevents the incompatible CPU features from being used.
	cpu := platform.CpuFeatures.Raw()
	// Reuse the `ret` buffer to write the first 8 bytes of the CPU features so that we can avoid the allocation.
	binary.LittleEndian.PutUint64(ret[:8], cpu)
	s.Write(ret[:8])
	// Finally, write the hash to the ret buffer.
	s.Sum(ret[:0])
	return
}

func (e *engine) addCompiledModule(module *wasm.Module, cm *compiledModule) (c *compiledModule, err error) {
	c = e.addCompiledModuleToMemory(module, cm)
	if !module.IsHostModule && e.fileCache != nil {
		err = e.addCompiledModuleToCache(module, c)
	}
	return
}

func (e *engine) getCompiledModule(module *wasm.Module, listeners []api.FunctionListener, ensureTermination bool) (cm *compiledModule, ok bool, err error) {
	cm, ok = e.getCompiledModuleFromMemory(module, true)
	if ok {
		return
	}
	cm, ok, err = e.getCompiledModuleFromCache(module)
	if ok {
		cm.parent = e
		cm.module = module
		cm.sharedFunctions = e.sharedFunctions
		cm.ensureTermination = ensureTermination
		cm.offsets = nativeapi.NewModuleContextOffsetData(module, len(listeners) > 0)
		if len(listeners) > 0 {
			cm.listeners = listeners
			cm.listenerBeforeTrampolines = make([]*byte, len(module.TypeSection))
			cm.listenerAfterTrampolines = make([]*byte, len(module.TypeSection))
			for i := range module.TypeSection {
				typ := &module.TypeSection[i]
				before, after := e.getListenerTrampolineForType(typ)
				cm.listenerBeforeTrampolines[i] = before
				cm.listenerAfterTrampolines[i] = after
			}
		}
		e.addCompiledModuleToMemory(module, cm)
		// ponytail: entry preambles are deserialized from the cache entry
		// above (see the entryPreambleFormatVersion section in
		// deserializeCompiledModule), so no recompile is needed here. One
		// thing intentionally left out on this hit path: PerfMap entries for
		// the preambles (nativeapi.PerfMap.AddEntry, see
		// compileEntryPreambles) are not re-registered, since PerfMap is
		// debug-only and off by default. If that's ever needed on a cache
		// hit, reconstruct the entries here from module.TypeSection +
		// cm.entryPreamblesPtrs when nativeapi.PerfMapEnabled is true.

		// Set the finalizer.
		e.setFinalizer(cm.executables, executablesFinalizer)
	}
	return
}

func (e *engine) addCompiledModuleToMemory(m *wasm.Module, cm *compiledModule) *compiledModule {
	e.mux.Lock()
	defer e.mux.Unlock()
	if c, ok := e.compiledModules[m.ID]; ok {
		c.refCount++
		return c.compiledModule
	}
	e.compiledModules[m.ID] = &compiledModuleWithCount{compiledModule: cm, refCount: 1}
	if len(cm.executable) > 0 {
		e.addCompiledModuleToSortedList(cm)
	}
	return cm
}

func (e *engine) getCompiledModuleFromMemory(module *wasm.Module, increaseRefCount bool) (cm *compiledModule, ok bool) {
	// Only the refCount bump requires exclusive access; a plain lookup can proceed
	// under a read lock since e.mux is a sync.RWMutex.
	if increaseRefCount {
		e.mux.Lock()
		defer e.mux.Unlock()
	} else {
		e.mux.RLock()
		defer e.mux.RUnlock()
	}

	cmWithCount, ok := e.compiledModules[module.ID]
	if ok {
		cm = cmWithCount.compiledModule
		if increaseRefCount {
			cmWithCount.refCount++
		}
	}
	return
}

func (e *engine) addCompiledModuleToCache(module *wasm.Module, cm *compiledModule) (err error) {
	if e.fileCache == nil || module.IsHostModule {
		return
	}
	err = e.fileCache.Add(fileCacheKey(module), serializeCompiledModule(e.wazyVersion, cm))
	return
}

func (e *engine) getCompiledModuleFromCache(module *wasm.Module) (cm *compiledModule, hit bool, err error) {
	if e.fileCache == nil || module.IsHostModule {
		return
	}

	// Check if the entries exist in the external cache.
	var cached io.ReadCloser
	cached, hit, err = e.fileCache.Get(fileCacheKey(module))
	if !hit || err != nil {
		return
	}

	// Otherwise, we hit the cache on external cache.
	// We retrieve *code structures from `cached`.
	var staleCache bool
	// Note: cached.Close is ensured to be called in deserializeCodes.
	cm, staleCache, err = deserializeCompiledModule(e.wazyVersion, cached)
	if err != nil {
		hit = false
		return
	} else if staleCache {
		return nil, false, e.fileCache.Delete(fileCacheKey(module))
	}
	return
}

var magic = []byte{'N', 'A', 'T', 'I', 'V', 'E'}

func serializeCompiledModule(wazyVersion string, cm *compiledModule) io.Reader {
	// The native code (cm.executable) is often hundreds of KiB; copying it
	// into a shared buffer just to serialize it would double the memory
	// traffic of every cache write. Instead we split the output into a head
	// (everything up to and including the code-segment length), the
	// executable itself streamed directly with zero extra copies, and a tail
	// (everything after the executable), then chain them with
	// io.MultiReader.
	// The head has a deterministic size, so presize it exactly to avoid the
	// buffer regrowth allocations.
	headSize := len(magic) + 1 /* version size */ + len(wazyVersion) +
		4 /* function count */ + 8*len(cm.functionOffsets) + 8 /* code length */
	head := bytes.NewBuffer(make([]byte, 0, headSize))
	// First 6 byte: NATIVE header.
	head.Write(magic)
	// Next 1 byte: length of version:
	head.WriteByte(byte(len(wazyVersion)))
	// Version of wazy.
	head.WriteString(wazyVersion)
	// Number of *code (== locally defined functions in the module): 4 bytes.
	head.Write(u32.LeBytes(uint32(len(cm.functionOffsets))))
	for _, offset := range cm.functionOffsets {
		// The offset of this function in the executable (8 bytes).
		head.Write(u64.LeBytes(uint64(offset)))
	}
	// The length of code segment (8 bytes).
	head.Write(u64.LeBytes(uint64(len(cm.executable))))

	tail := bytes.NewBuffer(nil)
	// Append checksum. Computed directly from cm.executable -- it doesn't
	// need to live in a buffer first.
	checksum := crc32.Checksum(cm.executable, crc)
	tail.Write(u32.LeBytes(checksum))
	if sm := cm.sourceMap; len(sm.executableOffsets) > 0 {
		tail.WriteByte(1) // indicates that source map is present.
		l := len(sm.wasmBinaryOffsets)
		tail.Write(u64.LeBytes(uint64(l)))
		executableAddr := uintptr(unsafe.Pointer(&cm.executable[0]))
		for i := 0; i < l; i++ {
			tail.Write(u64.LeBytes(sm.wasmBinaryOffsets[i]))
			// executableOffsets is absolute address, so we need to subtract executableAddr.
			tail.Write(u64.LeBytes(uint64(sm.executableOffsets[i] - executableAddr)))
		}
	} else {
		tail.WriteByte(0) // indicates that source map is not present.
	}
	// Try-table info: a format-version byte (guards this section's binary
	// layout independently, same pattern as ehTableFormatVersion below --
	// bumped when FloorSize was added), then number of try_tables (4
	// bytes), then for each: numLocals (4 bytes), reuseLocals (1 byte),
	// floorSize (4 bytes), clause count (4 bytes), then for each clause:
	// kind (1 byte) + tagIndex (4 bytes).
	tail.WriteByte(tryTableInfoFormatVersion)
	tail.Write(u32.LeBytes(uint32(len(cm.tryTableInfo))))
	for _, info := range cm.tryTableInfo {
		tail.Write(u32.LeBytes(uint32(info.NumLocals)))
		b := byte(0)
		if info.ReuseLocals {
			b = 1
		}
		tail.WriteByte(b)
		tail.Write(u32.LeBytes(uint32(info.FloorSize)))
		tail.Write(u32.LeBytes(uint32(len(info.CatchClauses))))
		for _, c := range info.CatchClauses {
			tail.WriteByte(c.Kind)
			tail.Write(u32.LeBytes(c.TagIndex))
		}
	}

	// Exception side table (per-function PC-range entries; see
	// nativeapi.EhEntry / docs/design/eh-side-table.md). Preceded by an
	// explicit format-version byte -- distinct from the wazyVersion check
	// above -- so that a cache entry written by an older build of this exact
	// section's layout is unambiguously detected as stale (rather than
	// misparsed) even if wazyVersion happens not to have changed. Layout:
	// ehTableFormatVersion (1 byte), numFunctions (4 bytes), then per
	// function: entry count (4 bytes), then per entry:
	// startOffset/endOffset (8 bytes each, executable-relative),
	// tryTableID (4 bytes), clause count (4 bytes), then per clause:
	// kind (1 byte) + tagIndex (4 bytes) + landingPad (8 bytes,
	// executable-relative).
	tail.WriteByte(ehTableFormatVersion)
	tail.Write(u32.LeBytes(uint32(len(cm.ehTables))))
	var executableAddr uintptr
	if len(cm.executable) > 0 {
		executableAddr = uintptr(unsafe.Pointer(&cm.executable[0]))
	}
	for _, entries := range cm.ehTables {
		tail.Write(u32.LeBytes(uint32(len(entries))))
		for _, e := range entries {
			tail.Write(u64.LeBytes(uint64(e.StartOffset - executableAddr)))
			tail.Write(u64.LeBytes(uint64(e.EndOffset - executableAddr)))
			tail.Write(u32.LeBytes(uint32(e.TryTableID)))
			tail.Write(u32.LeBytes(uint32(len(e.Clauses))))
			for _, c := range e.Clauses {
				tail.WriteByte(c.Kind)
				tail.Write(u32.LeBytes(c.TagIndex))
				tail.Write(u64.LeBytes(uint64(c.LandingPad - executableAddr)))
			}
		}
	}
	// functionFrameSizes: one int64 per local function (used on amd64 to
	// recover a throw-time ancestor frame's SP from its FP; see
	// nativeapi.ThrowFrame / resolveThrowTransferSPFP).
	tail.Write(u32.LeBytes(uint32(len(cm.functionFrameSizes))))
	for _, sz := range cm.functionFrameSizes {
		tail.Write(u64.LeBytes(uint64(sz)))
	}

	// Interrupt-check interval: a format-version byte then the interval (8
	// bytes). This is part of the module's compile identity and seeds the
	// runtime interruptCheckMask, so a cache-loaded module keeps the yield
	// frequency it was compiled with (rather than defaulting to 0 =
	// check-every-iteration). Own version byte so an older cache entry lacking
	// this section is detected as stale rather than misparsed.
	tail.WriteByte(interruptIntervalFormatVersion)
	tail.Write(u64.LeBytes(cm.interruptCheckInterval))

	// Entry preambles: position-independent Go->wasm trampolines, one per
	// entry in the module's TypeSection (see compileEntryPreambles). They're
	// pure register-relative code with no absolute-address fixups, so the
	// raw bytes cached here and re-mmapped on load are byte-identical to
	// recompiling them from the module's TypeSection -- this lets a cache hit
	// skip building an SSA builder/backend compiler entirely. Own format-
	// version byte, same rationale as interruptIntervalFormatVersion: a cache
	// entry written before this section existed EOFs here and is treated as
	// stale rather than misparsed. Layout: entryPreambleFormatVersion (1
	// byte), numPreambles (4 bytes), and if numPreambles > 0: numPreambles
	// per-preamble sizes (4 bytes each, including 16-byte alignment padding),
	// blob length (8 bytes), the raw preamble blob, then a CRC32 checksum (4
	// bytes) over the blob. The blob is executed code loaded from disk, so it
	// is checksummed exactly like cm.executable to reject corrupted/tampered
	// cache entries before they are mmapped.
	tail.WriteByte(entryPreambleFormatVersion)
	n := len(cm.entryPreamblesPtrs)
	tail.Write(u32.LeBytes(uint32(n)))
	if n > 0 {
		base := uintptr(unsafe.Pointer(&cm.entryPreambles[0]))
		for i := 0; i < n; i++ {
			start := uintptr(unsafe.Pointer(cm.entryPreamblesPtrs[i])) - base
			var end uintptr
			if i+1 < n {
				end = uintptr(unsafe.Pointer(cm.entryPreamblesPtrs[i+1])) - base
			} else {
				end = uintptr(len(cm.entryPreambles))
			}
			tail.Write(u32.LeBytes(uint32(end - start)))
		}
		tail.Write(u64.LeBytes(uint64(len(cm.entryPreambles))))
		tail.Write(cm.entryPreambles)
		// Checksum over the preamble blob, mirroring the executable checksum.
		tail.Write(u32.LeBytes(crc32.Checksum(cm.entryPreambles, crc)))
	}

	// *bytes.Buffer already implements io.Reader; MultiReader drains each once,
	// which is exactly the single-pass consumption fileCache.Add performs. Only
	// the executable needs a wrapping reader (it's a raw []byte). This avoids
	// the extra bytes.NewReader wrappers around the head/tail buffers.
	return io.MultiReader(head, bytes.NewReader(cm.executable), tail)
}

// ehTableFormatVersion guards the exception side-table cache section
// (cm.ehTables / cm.functionFrameSizes) independently of wazyVersion: bump
// this whenever that section's binary layout changes, so that old cache
// entries are detected as stale rather than misparsed.
const ehTableFormatVersion = 1

// tryTableInfoFormatVersion guards the try-table-info cache section
// (cm.tryTableInfo) independently of wazyVersion, same rationale as
// ehTableFormatVersion: bump whenever this section's binary layout
// changes (e.g. when TryTableInfo.FloorSize was added) so that old cache
// entries are detected as stale rather than misparsed.
const tryTableInfoFormatVersion = 1

// interruptIntervalFormatVersion guards the trailing interrupt-check-interval
// scalar, same rationale as ehTableFormatVersion: a cache entry written before
// this section existed hits EOF at the version byte and is treated as stale
// (forcing one recompile) rather than misparsed.
const interruptIntervalFormatVersion = 1

// entryPreambleFormatVersion guards the trailing entry-preambles cache
// section (cm.entryPreambles / cm.entryPreamblesPtrs), same rationale as
// ehTableFormatVersion / interruptIntervalFormatVersion: its own version byte
// means a cache entry written before this section existed (or under an
// incompatible layout) hits EOF/mismatch here and is treated as stale rather
// than misparsed.
const entryPreambleFormatVersion = 1

func deserializeCompiledModule(wazyVersion string, reader io.ReadCloser) (cm *compiledModule, staleCache bool, err error) {
	defer reader.Close()
	// Wrap the raw file in a buffered reader: without it, every 8-byte read (one per function
	// offset / source-map entry) is its own read(2) syscall.
	bufReader := bufio.NewReaderSize(reader, 64<<10)
	cacheHeaderSize := len(magic) + 1 /* version size */ + len(wazyVersion) + 4 /* number of functions */

	// Read the header before the native code.
	header := make([]byte, cacheHeaderSize)
	n, err := io.ReadFull(bufReader, header)
	if err != nil {
		return nil, false, fmt.Errorf("compilationcache: error reading header: %v", err)
	}

	if n != cacheHeaderSize {
		return nil, false, fmt.Errorf("compilationcache: invalid header length: %d", n)
	}

	if !bytes.Equal(header[:len(magic)], magic) {
		return nil, false, fmt.Errorf(
			"compilationcache: invalid magic number: got %s but want %s", magic, header[:len(magic)])
	}

	// Check the version compatibility.
	versionSize := int(header[len(magic)])

	cachedVersionBegin, cachedVersionEnd := len(magic)+1, len(magic)+1+versionSize
	if cachedVersionEnd >= len(header) {
		staleCache = true
		return
	} else if cachedVersion := string(header[cachedVersionBegin:cachedVersionEnd]); cachedVersion != wazyVersion {
		staleCache = true
		return
	}

	functionsNum := binary.LittleEndian.Uint32(header[len(header)-4:])
	cm = &compiledModule{functionOffsets: make([]int, functionsNum), executables: &executables{}}

	var eightBytes [8]byte
	for i := uint32(0); i < functionsNum; i++ {
		// Read the offset of each function in the executable.
		var offset uint64
		if offset, err = readUint64(bufReader, &eightBytes); err != nil {
			err = fmt.Errorf("compilationcache: error reading func[%d] executable offset: %v", i, err)
			return
		}
		cm.functionOffsets[i] = int(offset)
	}

	executableLen, err := readUint64(bufReader, &eightBytes)
	if err != nil {
		err = fmt.Errorf("compilationcache: error reading executable size: %v", err)
		return
	}

	if executableLen > 0 {
		executable, err := platform.MmapCodeSegment(int(executableLen))
		if err != nil {
			err = fmt.Errorf("compilationcache: error mmapping executable (len=%d): %v", executableLen, err)
			return nil, false, err
		}

		_, err = io.ReadFull(bufReader, executable)
		if err != nil {
			err = fmt.Errorf("compilationcache: error reading executable (len=%d): %v", executableLen, err)
			return nil, false, err
		}

		expected := crc32.Checksum(executable, crc)
		if _, err = io.ReadFull(bufReader, eightBytes[:4]); err != nil {
			return nil, false, fmt.Errorf("compilationcache: could not read checksum: %v", err)
		} else if checksum := binary.LittleEndian.Uint32(eightBytes[:4]); expected != checksum {
			return nil, false, fmt.Errorf("compilationcache: checksum mismatch (expected %d, got %d)", expected, checksum)
		}

		if err = platform.MprotectCodeSegment(executable); err != nil {
			return nil, false, err
		}
		cm.executable = executable
	}

	if _, err := io.ReadFull(bufReader, eightBytes[:1]); err != nil {
		return nil, false, fmt.Errorf("compilationcache: error reading source map presence: %v", err)
	}

	if eightBytes[0] == 1 {
		sm := &cm.sourceMap
		sourceMapLen, err := readUint64(bufReader, &eightBytes)
		if err != nil {
			err = fmt.Errorf("compilationcache: error reading source map length: %v", err)
			return nil, false, err
		}
		executableOffset := uintptr(unsafe.Pointer(&cm.executable[0]))
		for i := uint64(0); i < sourceMapLen; i++ {
			wasmBinaryOffset, err := readUint64(bufReader, &eightBytes)
			if err != nil {
				err = fmt.Errorf("compilationcache: error reading source map[%d] wasm binary offset: %v", i, err)
				return nil, false, err
			}
			executableRelativeOffset, err := readUint64(bufReader, &eightBytes)
			if err != nil {
				err = fmt.Errorf("compilationcache: error reading source map[%d] executable offset: %v", i, err)
				return nil, false, err
			}
			sm.wasmBinaryOffsets = append(sm.wasmBinaryOffsets, wasmBinaryOffset)
			// executableOffsets is absolute address, so we need to add executableOffset.
			sm.executableOffsets = append(sm.executableOffsets, uintptr(executableRelativeOffset)+executableOffset)
		}
	}
	// Try-table info -- see serializeCompiledModule's comment for the
	// layout. A missing/mismatched format-version byte means either a
	// pre-FloorSize cache entry or an incompatible layout version: treat as
	// stale (forces a recompile) rather than risk misparsing raw bytes as
	// this section's structure.
	if _, err = io.ReadFull(bufReader, eightBytes[:1]); err != nil || eightBytes[0] != tryTableInfoFormatVersion {
		return nil, true, nil
	}
	if _, err = io.ReadFull(bufReader, eightBytes[:4]); err != nil {
		return nil, true, nil
	}
	tableLen := binary.LittleEndian.Uint32(eightBytes[:4])
	if tableLen > 0 {
		cm.tryTableInfo = make([]nativeapi.TryTableInfo, tableLen)
		for i := uint32(0); i < tableLen; i++ {
			if _, err = io.ReadFull(bufReader, eightBytes[:5]); err != nil {
				return nil, false, fmt.Errorf("compilationcache: error reading try_table[%d] header: %v", i, err)
			}
			numLocals := int(binary.LittleEndian.Uint32(eightBytes[:4]))
			reuseLocals := eightBytes[4] != 0
			if _, err = io.ReadFull(bufReader, eightBytes[:4]); err != nil {
				return nil, false, fmt.Errorf("compilationcache: error reading floor size for try_table[%d]: %v", i, err)
			}
			floorSize := int(binary.LittleEndian.Uint32(eightBytes[:4]))
			if _, err = io.ReadFull(bufReader, eightBytes[:4]); err != nil {
				return nil, false, fmt.Errorf("compilationcache: error reading catch clause count for try_table[%d]: %v", i, err)
			}
			clauseCount := binary.LittleEndian.Uint32(eightBytes[:4])
			clauses := make([]nativeapi.CatchClauseInstance, clauseCount)
			for j := uint32(0); j < clauseCount; j++ {
				if _, err = io.ReadFull(bufReader, eightBytes[:5]); err != nil {
					return nil, false, fmt.Errorf("compilationcache: error reading catch clause[%d][%d]: %v", i, j, err)
				}
				clauses[j] = nativeapi.CatchClauseInstance{
					Kind:     eightBytes[0],
					TagIndex: binary.LittleEndian.Uint32(eightBytes[1:5]),
				}
			}
			cm.tryTableInfo[i] = nativeapi.TryTableInfo{
				CatchClauses: clauses,
				NumLocals:    numLocals,
				ReuseLocals:  reuseLocals,
				FloorSize:    floorSize,
			}
		}
	}

	// Exception side table -- see serializeCompiledModule's comment for the
	// layout. A missing/mismatched format-version byte means either a
	// pre-side-table cache entry or one written by an incompatible layout
	// version: either way, treat as stale (forces a recompile) rather than
	// risk misparsing raw bytes as this section's structure.
	if _, err = io.ReadFull(bufReader, eightBytes[:1]); err != nil || eightBytes[0] != ehTableFormatVersion {
		return nil, true, nil
	}
	if _, err = io.ReadFull(bufReader, eightBytes[:4]); err != nil {
		return nil, true, nil
	}
	numFuncs := binary.LittleEndian.Uint32(eightBytes[:4])
	if int(numFuncs) != len(cm.functionOffsets) {
		// Should be impossible (both are derived from the same module at
		// serialize time), but guards against silently misparsing a
		// corrupted/foreign cache entry as this section's structure.
		return nil, true, nil
	}
	var executableAddr uintptr
	if len(cm.executable) > 0 {
		executableAddr = uintptr(unsafe.Pointer(&cm.executable[0]))
	}
	cm.ehTables = make([][]nativeapi.EhEntry, numFuncs)
	for fnum := uint32(0); fnum < numFuncs; fnum++ {
		if _, err = io.ReadFull(bufReader, eightBytes[:4]); err != nil {
			return nil, false, fmt.Errorf("compilationcache: error reading eh table entry count for func[%d]: %v", fnum, err)
		}
		entryCount := binary.LittleEndian.Uint32(eightBytes[:4])
		if entryCount == 0 {
			continue
		}
		entries := make([]nativeapi.EhEntry, entryCount)
		for i := range entries {
			startOffset, err := readUint64(bufReader, &eightBytes)
			if err != nil {
				return nil, false, fmt.Errorf("compilationcache: error reading eh entry[%d][%d] start offset: %v", fnum, i, err)
			}
			endOffset, err := readUint64(bufReader, &eightBytes)
			if err != nil {
				return nil, false, fmt.Errorf("compilationcache: error reading eh entry[%d][%d] end offset: %v", fnum, i, err)
			}
			if _, err = io.ReadFull(bufReader, eightBytes[:4]); err != nil {
				return nil, false, fmt.Errorf("compilationcache: error reading eh entry[%d][%d] try-table id: %v", fnum, i, err)
			}
			tryTableID := int(binary.LittleEndian.Uint32(eightBytes[:4]))
			if _, err = io.ReadFull(bufReader, eightBytes[:4]); err != nil {
				return nil, false, fmt.Errorf("compilationcache: error reading eh entry[%d][%d] clause count: %v", fnum, i, err)
			}
			clauseCount := binary.LittleEndian.Uint32(eightBytes[:4])
			clauses := make([]nativeapi.EhClause, clauseCount)
			for j := range clauses {
				if _, err = io.ReadFull(bufReader, eightBytes[:1]); err != nil {
					return nil, false, fmt.Errorf("compilationcache: error reading eh clause[%d][%d][%d] kind: %v", fnum, i, j, err)
				}
				kind := eightBytes[0]
				if _, err = io.ReadFull(bufReader, eightBytes[:4]); err != nil {
					return nil, false, fmt.Errorf("compilationcache: error reading eh clause[%d][%d][%d] tag index: %v", fnum, i, j, err)
				}
				tagIndex := binary.LittleEndian.Uint32(eightBytes[:4])
				landingPad, err := readUint64(bufReader, &eightBytes)
				if err != nil {
					return nil, false, fmt.Errorf("compilationcache: error reading eh clause[%d][%d][%d] landing pad: %v", fnum, i, j, err)
				}
				clauses[j] = nativeapi.EhClause{Kind: kind, TagIndex: tagIndex, LandingPad: uintptr(landingPad) + executableAddr}
			}
			entries[i] = nativeapi.EhEntry{
				StartOffset: uintptr(startOffset) + executableAddr,
				EndOffset:   uintptr(endOffset) + executableAddr,
				TryTableID:  tryTableID,
				Clauses:     clauses,
			}
		}
		cm.ehTables[fnum] = entries
	}

	if _, err = io.ReadFull(bufReader, eightBytes[:4]); err != nil {
		return nil, false, fmt.Errorf("compilationcache: error reading function frame size count: %v", err)
	}
	frameSizeCount := binary.LittleEndian.Uint32(eightBytes[:4])
	cm.functionFrameSizes = make([]int64, frameSizeCount)
	for i := range cm.functionFrameSizes {
		sz, err := readUint64(bufReader, &eightBytes)
		if err != nil {
			return nil, false, fmt.Errorf("compilationcache: error reading function frame size[%d]: %v", i, err)
		}
		cm.functionFrameSizes[i] = int64(sz)
	}

	// Interrupt-check interval section (see serialize). A cache entry written
	// before this section existed EOFs here; treat that (and any short read or
	// version mismatch) as stale so it is recompiled rather than loaded with a
	// zero interval.
	if _, err = io.ReadFull(bufReader, eightBytes[:1]); err != nil || eightBytes[0] != interruptIntervalFormatVersion {
		return nil, true, nil
	}
	interval, err := readUint64(bufReader, &eightBytes)
	if err != nil {
		return nil, true, nil
	}
	cm.interruptCheckInterval = interval

	// Entry preambles section (see serialize). A cache entry written before
	// this section existed EOFs here; treat that (and any short read or
	// version mismatch) as stale so it is recompiled rather than loaded
	// without its preambles. The blob is executed code, so it carries a CRC32
	// checksum (verified before mmapping) exactly like cm.executable.
	if _, err = io.ReadFull(bufReader, eightBytes[:1]); err != nil || eightBytes[0] != entryPreambleFormatVersion {
		return nil, true, nil
	}
	if _, err = io.ReadFull(bufReader, eightBytes[:4]); err != nil {
		return nil, true, nil
	}
	numPreambles := binary.LittleEndian.Uint32(eightBytes[:4])
	if numPreambles > 0 {
		sizes := make([]int, numPreambles)
		for i := range sizes {
			if _, err = io.ReadFull(bufReader, eightBytes[:4]); err != nil {
				return nil, false, fmt.Errorf("compilationcache: error reading entry preamble[%d] size: %v", i, err)
			}
			sizes[i] = int(binary.LittleEndian.Uint32(eightBytes[:4]))
		}
		blobLen, err := readUint64(bufReader, &eightBytes)
		if err != nil {
			return nil, false, fmt.Errorf("compilationcache: error reading entry preamble blob length: %v", err)
		}
		blob := make([]byte, blobLen)
		if _, err = io.ReadFull(bufReader, blob); err != nil {
			return nil, false, fmt.Errorf("compilationcache: error reading entry preamble blob (len=%d): %v", blobLen, err)
		}
		// Verify the checksum before mmapping, mirroring the executable path.
		expected := crc32.Checksum(blob, crc)
		if _, err = io.ReadFull(bufReader, eightBytes[:4]); err != nil {
			return nil, false, fmt.Errorf("compilationcache: could not read entry preamble checksum: %v", err)
		} else if checksum := binary.LittleEndian.Uint32(eightBytes[:4]); expected != checksum {
			return nil, false, fmt.Errorf("compilationcache: entry preamble checksum mismatch (expected %d, got %d)", expected, checksum)
		}
		// Validate the per-preamble offsets against the blob length before
		// indexing into the mmapped code: a corrupted or foreign cache entry
		// must return stale (forcing a clean recompile) rather than panic on
		// &cm.entryPreambles[offset]. Every size must be non-negative and the
		// running offset must stay within [0, blobLen); the sizes must also
		// exactly cover the blob.
		offset := 0
		for _, size := range sizes {
			if size < 0 || offset < 0 || uint64(offset) >= blobLen {
				return nil, true, nil
			}
			offset += size
		}
		if uint64(offset) != blobLen {
			return nil, true, nil
		}
		cm.entryPreambles = mmapExecutable(blob)
		cm.entryPreamblesPtrs = make([]*byte, numPreambles)
		offset = 0
		for i, size := range sizes {
			cm.entryPreamblesPtrs[i] = &cm.entryPreambles[offset]
			offset += size
		}
	}

	return
}

// readUint64 strictly reads an uint64 in little-endian byte order, using the
// given array as a buffer. This returns io.EOF if nothing could be read, or
// io.ErrUnexpectedEOF if fewer than 8 bytes were available.
func readUint64(reader io.Reader, b *[8]byte) (uint64, error) {
	s := b[0:8]
	// io.ReadFull, not reader.Read: a plain Read may return fewer bytes than
	// requested without error (e.g. across an internal buffer boundary).
	if _, err := io.ReadFull(reader, s); err != nil {
		return 0, err
	}

	// Read the u64 from the underlying buffer.
	ret := binary.LittleEndian.Uint64(s)
	return ret, nil
}
