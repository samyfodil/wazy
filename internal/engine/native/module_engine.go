package native

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync/atomic"
	"unsafe"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
	"github.com/samyfodil/wazy/internal/internalapi"
	"github.com/samyfodil/wazy/internal/wasm"
	"github.com/samyfodil/wazy/internal/wasmdebug"
	"github.com/samyfodil/wazy/internal/wasmruntime"
)

type (
	// moduleEngine implements wasm.ModuleEngine.
	moduleEngine struct {
		// opaquePtr equals &opaque[0].
		opaquePtr *byte
		parent    *compiledModule
		module    *wasm.ModuleInstance
		opaque    moduleContextOpaque
		// localFunctionInstances is the head of the ref.func memo list. See localFuncref; a module
		// that never executes ref.func never allocates a node.
		localFunctionInstances atomic.Pointer[funcrefNode]
		importedFunctions      []importedFunction
		listeners              []api.FunctionListener
		// gcEnabled caches whether this runtime enables the GC proposal, so the per-call check in
		// callWithStack is one load off an already-hot pointer rather than three dependent ones.
		gcEnabled bool
		// gcRootsReserve is callEngine.gcRootsReserve's answer, precomputed. It is read twice on every
		// call -- to size the stack and to place its limit -- and deriving it there meant reaching
		// through to the compiled module for maxGCRoots on a path that otherwise never touches it.
		gcRootsReserve int32
	}

	functionInstance struct {
		executable             *byte
		moduleContextOpaquePtr *byte
		typeID                 wasm.FunctionTypeID
		indexInModule          wasm.Index
	}

	// funcrefNode is one memoized ref.func result. Its fi is addressed by the guest as a funcref, so
	// a node is never moved or freed while its module engine lives.
	funcrefNode struct {
		fi   functionInstance
		next *funcrefNode
	}

	importedFunction struct {
		me            *moduleEngine
		indexInModule wasm.Index
	}

	// moduleContextOpaque is the opaque byte slice of Module instance specific contents whose size
	// is only Wasm-compile-time known, hence dynamic. Its contents are basically the pointers to the module instance,
	// specific objects as well as functions. This is sometimes called "VMContext" in other Wasm runtimes.
	//
	// Internally, the buffer is structured as follows:
	//
	// 	type moduleContextOpaque struct {
	// 	    moduleInstance                            *wasm.ModuleInstance
	// 	    memories                                  [# of memories]memoryRecord (optional)
	// 	    importedFunctions                         [# of importedFunctions]functionInstance
	//      importedGlobals                           []ImportedGlobal       (optional)
	//      localGlobals                              []Global               (optional)
	//      typeIDsBegin                              &wasm.ModuleInstance.TypeIDs[0]  (optional)
	//      tables                                    []*wasm.TableInstance  (optional)
	// 	    beforeListenerTrampolines1stElement       **byte                 (optional)
	// 	    afterListenerTrampolines1stElement        **byte                 (optional)
	//      dataInstances1stElement                   []wasm.DataInstance    (optional)
	//      elementInstances1stElement                []wasm.ElementInstance (optional)
	// 	}
	//
	//  // memoryRecord is one per entry in the module's memory index space
	//  // (imports first, then module-defined). For an imported memory it holds
	//  // {*wasm.MemoryInstance, owner's opaque module context ptr, _ padding};
	//  // for a local memory it holds {buffer base pointer, byte length,
	//  // *wasm.MemoryInstance}. See nativeapi.MemoryOffset /
	//  // LocalMemoryInstancePtrOffset.
	//  type memoryRecord struct {
	// 		a, b, c uint64
	//  }
	//
	//  type ImportedGlobal struct {
	// 		*Global
	// 		_ uint64 // padding
	//  }
	//
	//  type Global struct {
	// 		Val, ValHi uint64
	//  }
	//
	// See nativeapi.NewModuleContextOffsetData for the details of the offsets.
	//
	// Note that for host modules, the structure is entirely different. See buildHostModuleOpaque.
	moduleContextOpaque []byte
)

func newAlignedOpaque(size int) moduleContextOpaque {
	// Check if the size is a multiple of 16.
	if size%16 != 0 {
		panic("size must be a multiple of 16")
	}
	buf := make([]byte, size+16)
	// Align the buffer to 16 bytes.
	rem := uintptr(unsafe.Pointer(&buf[0])) % 16
	buf = buf[16-rem:]
	return buf
}

func (m *moduleEngine) setupOpaque() {
	inst := m.module
	offsets := &m.parent.offsets
	opaque := m.opaque

	binary.LittleEndian.PutUint64(opaque[offsets.ModuleInstanceOffset:],
		uint64(uintptr(unsafe.Pointer(m.module))),
	)

	if offsets.MemoriesBegin >= 0 {
		for i := range inst.Memories {
			if wasm.Index(i) >= inst.Source.ImportMemoryCount {
				m.putLocalMemory(wasm.Index(i))
			}
			// Note: imported memories are resolved in ResolveImportedMemory.
		}
	}

	// Note: imported functions are resolved in ResolveImportedFunction.

	if globalOffset := offsets.GlobalsBegin; globalOffset >= 0 {
		for i, g := range inst.Globals {
			if i < int(inst.Source.ImportGlobalCount) {
				importedME := g.Me.(*moduleEngine)
				offset := importedME.parent.offsets.GlobalInstanceOffset(g.Index)
				importedMEOpaque := importedME.opaque
				binary.LittleEndian.PutUint64(opaque[globalOffset:],
					uint64(uintptr(unsafe.Pointer(&importedMEOpaque[offset]))))
			} else {
				binary.LittleEndian.PutUint64(opaque[globalOffset:], g.Val)
				binary.LittleEndian.PutUint64(opaque[globalOffset+8:], g.ValHi)
			}
			globalOffset += 16
		}
	}

	if tableOffset := offsets.TablesBegin; tableOffset >= 0 {
		// First we write the first element's address of typeIDs.
		if len(inst.TypeIDs) > 0 {
			binary.LittleEndian.PutUint64(opaque[offsets.TypeIDs1stElement:], uint64(uintptr(unsafe.Pointer(&inst.TypeIDs[0]))))
		}

		// Then we write the table addresses.
		for _, table := range inst.Tables {
			binary.LittleEndian.PutUint64(opaque[tableOffset:], uint64(uintptr(unsafe.Pointer(table))))
			tableOffset += 8
		}
	}

	if tagOffset := offsets.TagsBegin; tagOffset >= 0 {
		for _, tag := range inst.Tags {
			binary.LittleEndian.PutUint64(opaque[tagOffset:],
				uint64(uintptr(unsafe.Pointer(tag))))
			tagOffset += 8
		}
	}

	if beforeListenerOffset := offsets.BeforeListenerTrampolines1stElement; beforeListenerOffset >= 0 {
		binary.LittleEndian.PutUint64(opaque[beforeListenerOffset:], uint64(uintptr(unsafe.Pointer(&m.parent.listenerBeforeTrampolines[0]))))
	}
	if afterListenerOffset := offsets.AfterListenerTrampolines1stElement; afterListenerOffset >= 0 {
		binary.LittleEndian.PutUint64(opaque[afterListenerOffset:], uint64(uintptr(unsafe.Pointer(&m.parent.listenerAfterTrampolines[0]))))
	}
	if len(inst.DataInstances) > 0 {
		binary.LittleEndian.PutUint64(opaque[offsets.DataInstances1stElement:], uint64(uintptr(unsafe.Pointer(&inst.DataInstances[0]))))
	}
	if len(inst.ElementInstances) > 0 {
		binary.LittleEndian.PutUint64(opaque[offsets.ElementInstances1stElement:], uint64(uintptr(unsafe.Pointer(&inst.ElementInstances[0]))))
	}
}

// NewFunction implements wasm.ModuleEngine.
func (m *moduleEngine) NewFunction(index wasm.Index) api.Function {
	if nativeapi.PrintMachineCodeHexPerFunctionDisassemblable {
		panic("When PrintMachineCodeHexPerFunctionDisassemblable enabled, functions must not be called")
	}

	localIndex := index
	if importedFnCount := m.module.Source.ImportFunctionCount; index < importedFnCount {
		imported := &m.importedFunctions[index]
		return imported.me.NewFunction(imported.indexInModule)
	} else {
		localIndex -= importedFnCount
	}

	if source := m.module.Source; source.IsHostModule {
		// For host modules, we need to look up the GoFunction from the CodeSection.
		def := source.FunctionDefinition(localIndex)
		goF := source.CodeSection[localIndex].GoFunc
		switch typed := goF.(type) {
		case api.GoFunction:
			// GoFunction doesn't need looked up module.
			return &hostFunction{def: def, g: goFunctionAsGoModuleFunction(typed)}
		case api.GoModuleFunction:
			return &hostFunction{def: def, lookedUpModule: m.module, g: typed}
		default:
			panic(fmt.Sprintf("unexpected GoFunc type: %T", goF))
		}
	}

	src := m.module.Source
	typIndex := src.FunctionSection[localIndex]
	typ := src.TypeSection[typIndex]
	sizeOfParamResultSlice := typ.ResultNumInUint64
	if ps := typ.ParamNumInUint64; ps > sizeOfParamResultSlice {
		sizeOfParamResultSlice = ps
	}
	p := m.parent
	offset := p.functionOffsets[localIndex]

	ce := &callEngine{
		indexInModule:          index,
		executable:             &p.executable[offset],
		parent:                 m,
		preambleExecutable:     p.entryPreamblesPtrs[typIndex],
		sizeOfParamResultSlice: sizeOfParamResultSlice,
		requiredParams:         typ.ParamNumInUint64,
		numberOfResults:        typ.ResultNumInUint64,
	}

	if p.interruptCheckInterval != 0 {
		ce.execCtx.interruptCheckMask = p.interruptCheckInterval - 1
	}

	sharedFunctions := p.sharedFunctions
	ce.execCtx.memoryGrowTrampolineAddress = sharedFunctions.memoryGrowAddress
	ce.execCtx.stackGrowCallTrampolineAddress = sharedFunctions.stackGrowAddress
	ce.execCtx.checkModuleExitCodeTrampolineAddress = sharedFunctions.checkModuleExitCodeAddress
	ce.execCtx.tableGrowTrampolineAddress = sharedFunctions.tableGrowAddress
	ce.execCtx.refFuncTrampolineAddress = sharedFunctions.refFuncAddress
	ce.execCtx.memoryWait32TrampolineAddress = sharedFunctions.memoryWait32Address
	ce.execCtx.memoryWait64TrampolineAddress = sharedFunctions.memoryWait64Address
	ce.execCtx.memoryNotifyTrampolineAddress = sharedFunctions.memoryNotifyAddress
	ce.execCtx.throwAllocTrampolineAddress = sharedFunctions.throwAllocTrampolineAddress
	ce.execCtx.throwTrampolineAddress = sharedFunctions.throwTrampolineAddress
	ce.execCtx.tryTableEnterTrampolineAddress = sharedFunctions.tryTableEnterAddress
	ce.execCtx.gcCheckTrampolineAddress = sharedFunctions.gcCheckAddress
	ce.execCtx.tryTableLeaveTrampolineAddress = sharedFunctions.tryTableLeaveAddress
	ce.execCtx.memmoveAddress = memmovPtr
	ce.init()
	return ce
}

// GetGlobalValue implements the same method as documented on wasm.ModuleEngine.
func (m *moduleEngine) GetGlobalValue(i wasm.Index) (lo, hi uint64) {
	offset := m.parent.offsets.GlobalInstanceOffset(i)
	buf := m.opaque[offset:]
	if i < m.module.Source.ImportGlobalCount {
		panic("GetGlobalValue should not be called for imported globals")
	}
	return binary.LittleEndian.Uint64(buf), binary.LittleEndian.Uint64(buf[8:])
}

// SetGlobalValue implements the same method as documented on wasm.ModuleEngine.
func (m *moduleEngine) SetGlobalValue(i wasm.Index, lo, hi uint64) {
	offset := m.parent.offsets.GlobalInstanceOffset(i)
	buf := m.opaque[offset:]
	if i < m.module.Source.ImportGlobalCount {
		panic("GetGlobalValue should not be called for imported globals")
	}
	binary.LittleEndian.PutUint64(buf, lo)
	binary.LittleEndian.PutUint64(buf[8:], hi)
}

// OwnsGlobals implements the same method as documented on wasm.ModuleEngine.
func (m *moduleEngine) OwnsGlobals() bool { return true }

// MemoryGrown implements wasm.ModuleEngine.
func (m *moduleEngine) MemoryGrown(index wasm.Index) {
	m.putLocalMemory(index)
}

// putLocalMemory writes the index-th local memory's buffer pointer and length to the opaque buffer.
func (m *moduleEngine) putLocalMemory(index wasm.Index) {
	mem := m.module.Memories[index]
	offset := m.parent.offsets.MemoryOffset(int(index))

	s := mem.ByteSize()
	var b uint64
	if data := unsafe.SliceData(mem.Buffer); data != nil {
		b = uint64(uintptr(unsafe.Pointer(data)))
	}
	binary.LittleEndian.PutUint64(m.opaque[offset:], b)
	binary.LittleEndian.PutUint64(m.opaque[offset+8:], s)
	// The *wasm.MemoryInstance itself never changes across a Grow, but is
	// cheap to rewrite here rather than special-cased to instantiation-time
	// only; lowerLocalMemoryGrow's fast path reads it directly off this
	// offset to reach nativeGrowCap/sizeBytes without chasing the
	// ModuleInstance -> Memories slice.
	binary.LittleEndian.PutUint64(m.opaque[offset+16:], uint64(uintptr(unsafe.Pointer(mem))))
}

// ResolveImportedFunction implements wasm.ModuleEngine.
func (m *moduleEngine) ResolveImportedFunction(index, descFunc, indexInImportedModule wasm.Index, importedModuleEngine wasm.ModuleEngine) {
	executableOffset, moduleCtxOffset, typeIDOffset := m.parent.offsets.ImportedFunctionOffset(index)
	importedME := importedModuleEngine.(*moduleEngine)

	if int(indexInImportedModule) < len(importedME.importedFunctions) {
		imported := &importedME.importedFunctions[indexInImportedModule]
		m.ResolveImportedFunction(index, descFunc, imported.indexInModule, imported.me)
		return // Recursively resolve the imported function.
	}

	offset := importedME.parent.functionOffsets[indexInImportedModule-wasm.Index(len(importedME.importedFunctions))]
	typeID := m.module.TypeIDs[descFunc]
	executable := &importedME.parent.executable[offset]
	// Write functionInstance.
	binary.LittleEndian.PutUint64(m.opaque[executableOffset:], uint64(uintptr(unsafe.Pointer(executable))))
	binary.LittleEndian.PutUint64(m.opaque[moduleCtxOffset:], uint64(uintptr(unsafe.Pointer(importedME.opaquePtr))))
	binary.LittleEndian.PutUint64(m.opaque[typeIDOffset:], uint64(typeID))

	// Write importedFunction so that it can be used by NewFunction.
	m.importedFunctions[index] = importedFunction{me: importedME, indexInModule: indexInImportedModule}
}

// ResolveImportedMemory implements wasm.ModuleEngine.
func (m *moduleEngine) ResolveImportedMemory(index, indexInImportedModule wasm.Index, importedModuleEngine wasm.ModuleEngine) {
	importedME := importedModuleEngine.(*moduleEngine)
	inst := importedME.module

	var memInstPtr uint64
	var memOwnerOpaquePtr uint64
	if indexInImportedModule < inst.Source.ImportMemoryCount {
		// The imported module itself imports this memory: read its already-resolved
		// record so the chain of re-exports ultimately points at the true owner.
		offset := importedME.parent.offsets.MemoryOffset(int(indexInImportedModule))
		memInstPtr = binary.LittleEndian.Uint64(importedME.opaque[offset:])
		memOwnerOpaquePtr = binary.LittleEndian.Uint64(importedME.opaque[offset+8:])
	} else {
		memInstPtr = uint64(uintptr(unsafe.Pointer(inst.Memories[indexInImportedModule])))
		memOwnerOpaquePtr = uint64(uintptr(unsafe.Pointer(importedME.opaquePtr)))
	}
	offset := m.parent.offsets.MemoryOffset(int(index))
	binary.LittleEndian.PutUint64(m.opaque[offset:], memInstPtr)
	binary.LittleEndian.PutUint64(m.opaque[offset+8:], memOwnerOpaquePtr)
}

// DoneInstantiation implements wasm.ModuleEngine.
func (m *moduleEngine) DoneInstantiation() {
	if !m.module.Source.IsHostModule {
		m.setupOpaque()
	}
}

// TypeIDOfReference implements wasm.ModuleEngine. A funcref is the address of the callee's functionInstance
// in some module's opaque context, so its type is a plain field read.
func (m *moduleEngine) TypeIDOfReference(ref wasm.Reference) wasm.FunctionTypeID {
	return nativeapi.PtrFromUintptr[functionInstance](uintptr(ref)).typeID
}

// FunctionInstanceReference implements wasm.ModuleEngine.
func (m *moduleEngine) FunctionInstanceReference(funcIndex wasm.Index) wasm.Reference {
	if funcIndex < m.module.Source.ImportFunctionCount {
		begin, _, _ := m.parent.offsets.ImportedFunctionOffset(funcIndex)
		return uintptr(unsafe.Pointer(&m.opaque[begin]))
	}
	return m.localFuncref(funcIndex)
}

// localFuncref memoizes one functionInstance per local function index a funcref is actually taken
// of. Materializing one per execution allocated per instruction and appended to a module-lifetime
// slice that was never trimmed, so a guest looping on ref.func grew the engine without bound; that
// append also raced when two goroutines called into one instance.
//
// The memo is a prepend-only linked list rather than a table for two reasons. A funcref is a bare
// uintptr the GC does not trace, so an address handed to the guest must never move afterwards, and a
// node never does. And a list costs exactly one allocation per DISTINCT funcref -- the same as the
// per-execution code it replaces paid for its first one -- where a slice would also pay to publish
// each append, and a dense table indexed by function index would cost every instance 24 bytes per
// function to hold the handful of entries a module actually takes (a TinyGo module exporting ~90
// functions takes 8).
//
// ponytail: lookup is linear in the number of distinct funcrefs this instance has taken, which is a
// handful for every module shape seen here. A module taking hundreds would want a dense table keyed
// by the declared-function set (internal/wasm computes declaredFunctionIndexes during validation but
// does not retain it); switch to that if one ever shows up.
func (m *moduleEngine) localFuncref(funcIndex wasm.Index) wasm.Reference {
	for {
		head := m.localFunctionInstances.Load()
		for n := head; n != nil; n = n.next {
			if n.fi.indexInModule == funcIndex {
				return uintptr(unsafe.Pointer(&n.fi))
			}
		}
		p, src := m.parent, m.module.Source
		localIndex := funcIndex - src.ImportFunctionCount
		n := &funcrefNode{next: head, fi: functionInstance{
			executable:             &p.executable[p.functionOffsets[localIndex]],
			moduleContextOpaquePtr: m.opaquePtr,
			typeID:                 m.module.TypeIDs[src.FunctionSection[localIndex]],
			indexInModule:          funcIndex,
		}}
		// A racing writer that prepended first is handled by looping: the rescan may find it added
		// this very index, in which case that node wins and this one is dropped, so one index keeps
		// yielding one reference.
		if m.localFunctionInstances.CompareAndSwap(head, n) {
			return uintptr(unsafe.Pointer(&n.fi))
		}
	}
}

// LookupFunction implements wasm.ModuleEngine.
func (m *moduleEngine) LookupFunction(t *wasm.TableInstance, typeId wasm.FunctionTypeID, tableOffset wasm.Index) (*wasm.ModuleInstance, wasm.Index) {
	if tableOffset >= uint32(len(t.References)) || t.Type != wasm.RefTypeFuncref {
		panic(wasmruntime.ErrRuntimeInvalidTableAccess)
	}
	rawPtr := t.References[tableOffset]
	if rawPtr == 0 {
		panic(wasmruntime.ErrRuntimeInvalidTableAccess)
	}

	tf := nativeapi.PtrFromUintptr[functionInstance](rawPtr)
	if tf.typeID != typeId {
		panic(wasmruntime.ErrRuntimeIndirectCallTypeMismatch)
	}
	return moduleInstanceFromOpaquePtr(tf.moduleContextOpaquePtr), tf.indexInModule
}

func moduleInstanceFromOpaquePtr(ptr *byte) *wasm.ModuleInstance {
	return *(**wasm.ModuleInstance)(unsafe.Pointer(ptr))
}

type hostFunction struct {
	internalapi.WazyOnly
	def            *wasm.FunctionDefinition
	lookedUpModule *wasm.ModuleInstance
	g              api.GoModuleFunction
}

// goFunctionAsGoModuleFunction converts api.GoFunction to api.GoModuleFunction which ignores the api.Module argument.
func goFunctionAsGoModuleFunction(g api.GoFunction) api.GoModuleFunction {
	return api.GoModuleFunc(func(ctx context.Context, _ api.Module, stack []uint64) {
		g.Call(ctx, stack)
	})
}

// Definition implements api.Function.
func (f *hostFunction) Definition() api.FunctionDefinition { return f.def }

// Call implements api.Function.
func (f *hostFunction) Call(ctx context.Context, params ...uint64) ([]uint64, error) {
	typ := f.def.Functype
	stackSize := typ.ParamNumInUint64
	rn := typ.ResultNumInUint64
	if rn > stackSize {
		stackSize = rn
	}
	stack := make([]uint64, stackSize)
	copy(stack, params)
	return stack[:rn], f.CallWithStack(ctx, stack)
}

// CallWithStack implements api.Function.
func (f *hostFunction) CallWithStack(ctx context.Context, stack []uint64) (err error) {
	defer func() {
		if r := recover(); r != nil {
			builder := wasmdebug.NewErrorBuilder()
			err = builder.FromRecovered(r)
		}
	}()
	f.g.Call(ctx, f.lookedUpModule, stack)
	return nil
}
