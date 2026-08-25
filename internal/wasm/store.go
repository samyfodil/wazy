package wasm

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/expctxkeys"
	"github.com/samyfodil/wazy/internal/internalapi"
	internalsys "github.com/samyfodil/wazy/internal/sys"
	"github.com/samyfodil/wazy/sys"
)

// nameToModuleShrinkThreshold is the size the nameToModule map can grow to
// before it starts to be monitored for shrinking.
// The capacity will never be smaller than this once the threshold is met.
const nameToModuleShrinkThreshold = 100

type (
	// Store is the runtime representation of "instantiated" Wasm module and objects.
	// Multiple modules can be instantiated within a single store, and each instance,
	// (e.g. function instance) can be referenced by other module instances in a Store via Module.ImportSection.
	//
	// Every type whose name ends with "Instance" suffix belongs to exactly one store.
	//
	// Note that store is not thread (concurrency) safe, meaning that using single Store
	// via multiple goroutines might result in race conditions. In that case, the invocation
	// and access to any methods and field of Store must be guarded by mutex.
	//
	// See https://www.w3.org/TR/2019/REC-wasm-core-1-20191205/#store%E2%91%A0
	Store struct {
		// moduleList ensures modules are closed in reverse initialization order.
		moduleList *ModuleInstance // guarded by mux

		// nameToModule holds the instantiated Wasm modules by module name from Instantiate.
		// It ensures no race conditions instantiating two modules of the same name.
		nameToModule map[string]*ModuleInstance // guarded by mux

		// nameToModuleCap tracks the growth of the nameToModule map in order to
		// track when to shrink it.
		nameToModuleCap int // guarded by mux

		// EnabledFeatures are read-only to allow optimizations.
		EnabledFeatures api.CoreFeatures

		// Engine is a global context for a Store which is in responsible for compilation and execution of Wasm modules.
		Engine Engine

		// MemoryLimitPages and Memory64LimitPages cap how many pages a memory
		// may actually occupy, for a memory indexed by i32 and by i64
		// respectively. They mirror the embedder's WithMemoryLimitPages and
		// WithMemory64LimitPages.
		//
		// The binary decoder already applies MemoryLimitPages to a 32-bit
		// memory's declared maximum, so that one only matters here as a
		// belt-and-braces check. A 64-bit memory is different: the spec lets a
		// module declare up to Memory64LimitPages (2^48) pages and requires
		// such a module to *validate*, so its declared limits cannot be capped
		// at decode time without rejecting conformant modules. The ceiling is
		// therefore applied at instantiation: a minimum over it fails to
		// instantiate, and a maximum over it simply stops growth earlier.
		//
		// Both are set by NewStore and are taken at face value: zero means no
		// memory may hold a page, matching what WithMemoryLimitPages(0)
		// already meant for a 32-bit memory by way of the decoder.
		MemoryLimitPages, Memory64LimitPages uint64

		// typeIDs maps each FunctionType.String() to a unique FunctionTypeID. This is used at runtime to
		// do type-checks on indirect function calls.
		typeIDs map[string]FunctionTypeID

		// Exceptions hands out the exnref values guest code holds, and keeps the exceptions they name alive. It
		// is per-Store because an exnref crosses module instances. See ExceptionTable.
		Exceptions ExceptionTable

		// functionMaxTypes represents the limit on the number of function types in a store.
		// Note: this is fixed to 2^27 but have this a field for testability.
		functionMaxTypes uint32

		// mux is used to guard the fields from concurrent access.
		mux sync.RWMutex
	}

	// ModuleInstance represents instantiated wasm module.
	// The difference from the spec is that in wazy, a ModuleInstance holds pointers
	// to the instances, rather than "addresses" (i.e. index to Store.Functions, Globals, etc) for convenience.
	//
	// See https://www.w3.org/TR/2019/REC-wasm-core-1-20191205/#syntax-moduleinst
	//
	// This implements api.Module.
	ModuleInstance struct {
		internalapi.WazyOnlyType

		ModuleName string
		Exports    map[string]*Export
		Globals    []*GlobalInstance
		Memories   []*MemoryInstance
		Tables     []*TableInstance
		Tags       []*TagInstance

		// Engine implements function calls for this module.
		Engine ModuleEngine

		// TypeIDs is index-correlated with types and holds typeIDs which is uniquely assigned to a type by store.
		// This is necessary to achieve fast runtime type checking for indirect function calls at runtime.
		TypeIDs []FunctionTypeID

		// DataInstances holds data segments bytes of the module.
		// This is only used by bulk memory operations.
		//
		// https://www.w3.org/TR/2022/WD-wasm-core-2-20220419/exec/runtime.html#data-instances
		DataInstances []DataInstance

		// ElementInstances holds the element instance, and each holds the references to either functions
		// or external objects (unimplemented).
		ElementInstances []ElementInstance

		// Sys is exposed for use in special imports such as WASI, assemblyscript.
		//
		// # Notes
		//
		//   - This is a part of ModuleInstance so that scope and Close is coherent.
		//   - This is not exposed outside this repository (as a host function
		//	  parameter) because we haven't thought through capabilities based
		//	  security implications.
		Sys *internalsys.Context

		// Closed is used both to guard moduleEngine.CloseWithExitCode and to store the exit code.
		//
		// The update value is closedType + exitCode << 32. This ensures an exit code of zero isn't mistaken for never closed.
		//
		// Note: Exclusively reading and updating this with atomics guarantees cross-goroutine observations.
		// See /RATIONALE.md
		Closed atomic.Uint64

		// CodeCloser is non-nil when the code should be closed after this module.
		CodeCloser api.Closer

		// s is the Store on which this module is instantiated.
		s *Store
		// prev and next hold the nodes in the linked list of ModuleInstance held by Store.
		prev, next *ModuleInstance
		// Source is a pointer to the Module from which this ModuleInstance derives.
		Source *Module

		// CloseNotifier is an experimental hook called once on close.
		CloseNotifier api.CloseNotifier
	}

	// DataInstance holds bytes corresponding to the data segment in a module.
	//
	// https://www.w3.org/TR/2022/WD-wasm-core-2-20220419/exec/runtime.html#data-instances
	DataInstance = []byte
)

type (

	// GlobalInstance represents a global instance in a store.
	// See https://www.w3.org/TR/2019/REC-wasm-core-1-20191205/#global-instances%E2%91%A0
	GlobalInstance struct {
		Type GlobalType
		// Val holds a 64-bit representation of the actual value.
		// If me is non-nil, the value will not be updated and the current value is stored in the module engine.
		Val uint64
		// ValHi is only used for vector type globals, and holds the higher bits of the vector.
		// If me is non-nil, the value will not be updated and the current value is stored in the module engine.
		ValHi uint64
		// Me is the module engine that owns this global instance.
		// The .Val and .ValHi fields are only valid when me is nil.
		// If me is non-nil, the value is stored in the module engine.
		Me    ModuleEngine
		Index Index
	}

	// TagInstance represents an instantiated exception handling tag.
	// Tags are compared by identity (pointer equality), not structural type equality.
	TagInstance struct {
		// Type is the function type of this tag (params only; results must be empty).
		Type *FunctionType
	}

	// FunctionTypeID is a uniquely assigned integer for a function type.
	// This is wazy specific runtime object and specific to a store,
	// and used at runtime to do type-checks on indirect function calls.
	FunctionTypeID uint32
)

// The wazy specific limitations described at RATIONALE.md.
const maximumFunctionTypes = 1 << 27

// ExceptionTable returns the store-wide table naming live exceptions. Engines
// use it to turn an Exception into the exnref guest code holds, and back. See
// ExceptionTable.
func (m *ModuleInstance) ExceptionTable() *ExceptionTable {
	return &m.s.Exceptions
}

// GetFunctionTypeID is used by emscripten.
func (m *ModuleInstance) GetFunctionTypeID(t *FunctionType) FunctionTypeID {
	id, err := m.s.GetFunctionTypeID(t)
	if err != nil {
		// This is not recoverable in practice since the only error GetFunctionTypeID returns is
		// when there's too many function types in the store.
		panic(err)
	}
	return id
}

func (m *ModuleInstance) buildElementInstances(elements []ElementSegment) {
	m.ElementInstances = make([][]Reference, len(elements))
	for i := range elements {
		elm := &elements[i]
		if elm.Type.Kind() == RefTypeFuncref.Kind() && elm.Mode == ElementModePassive {
			// Only passive elements can be access as element instances.
			// See https://www.w3.org/TR/2022/WD-wasm-core-2-20220419/syntax/modules.html#element-segments
			inst := make([]Reference, len(elm.Init))
			m.ElementInstances[i] = inst
			for j := range elm.Init {
				inst[j] = evaluateElementInitInModuleInstance(elm, j, m)
			}
		}
	}
}

func (m *ModuleInstance) applyElements(elems []ElementSegment) {
	for elemI := range elems {
		elem := &elems[elemI]
		if !elem.IsActive() ||
			// Per https://github.com/WebAssembly/spec/issues/1427 init can be no-op.
			len(elem.Init) == 0 {
			continue
		}
		offsetExprResults := evaluateConstExprInModuleInstance(&elem.OffsetExpr, m)
		offset := offsetExprResults[0]

		table := m.Tables[elem.TableIndex]
		references := table.References
		// rangeOutOfBounds rather than a plain addition: a 64-bit table's offset
		// spans the whole uint64 range, so the sum can wrap into a value that
		// looks in bounds. It also keeps the offset out of an int, which is
		// negative for anything past MaxInt32 where an int is 32 bits wide.
		if rangeOutOfBounds(offset, uint64(len(elem.Init)), uint64(len(references))) {
			// ErrElementOffsetOutOfBounds is the error raised when the active element offset exceeds the table length.
			// Before CoreFeatureReferenceTypes, this was checked statically before instantiation, after the proposal,
			// this must be raised as runtime error (as in assert_trap in spectest), not even an instantiation error.
			// https://github.com/WebAssembly/spec/blob/d39195773112a22b245ffbe864bab6d1182ccb06/test/core/linking.wast#L264-L274
			//
			// In wazy, we ignore it since in any way, the instantiated module and engines are fine and can be used
			// for function invocations.
			return
		}

		if table.Type == RefTypeExternref {
			for i := 0; i < len(elem.Init); i++ {
				references[offset+uint64(i)] = Reference(0)
			}
		} else {
			for i := range elem.Init {
				references[offset+uint64(i)] = evaluateElementInitInModuleInstance(elem, i, m)
			}
		}
	}
}

// validateData ensures that data segments are valid in terms of memory boundary.
// Note: this is used only when bulk-memory/reference type feature is disabled.
func (m *ModuleInstance) validateData(data []DataSegment) (err error) {
	for i := range data {
		d := &data[i]
		if !d.IsPassive() {
			results, typ, err := evaluateConstExpr(
				&d.OffsetExpression,
				func(globalIndex Index) (ValueType, uint64, uint64, error) {
					if globalIndex >= Index(len(m.Globals)) {
						return 0, 0, 0, errors.New("global index out of range")
					}
					g := m.Globals[globalIndex]
					return g.Type.ValType, g.Val, g.ValHi, nil
				},
				func(funcIndex Index) (Reference, error) {
					return m.Engine.FunctionInstanceReference(funcIndex), nil
				},
			)
			if err != nil {
				return fmt.Errorf("%s[%d] failed to evaluate offset expression: %w", SectionIDName(SectionIDData), i, err)
			}
			if d.MemoryIndex >= uint32(len(m.Memories)) {
				return fmt.Errorf("%s[%d]: unknown memory %d", SectionIDName(SectionIDData), i, d.MemoryIndex)
			}
			mem := m.Memories[d.MemoryIndex]
			if expected := mem.indexType(); typ != expected {
				return fmt.Errorf("%s[%d] offset expression must return %s but was %s",
					SectionIDName(SectionIDData), i, ValueTypeName(expected), ValueTypeName(typ))
			}
			if rangeOutOfBounds(results[0], uint64(len(d.Init)), uint64(len(mem.Buffer))) {
				return fmt.Errorf("%s[%d]: out of bounds memory access", SectionIDName(SectionIDData), i)
			}
		}
	}
	return
}

// applyData uses the given data segments and mutate the memory according to the initial contents on it
// and populate the `DataInstances`. This is called after all the validation phase passes and out of
// bounds memory access error here is not a validation error, but rather a runtime error.
func (m *ModuleInstance) applyData(data []DataSegment) error {
	m.DataInstances = make([][]byte, len(data))
	for i := range data {
		d := &data[i]
		m.DataInstances[i] = d.Init
		if !d.IsPassive() {
			offsetExprResults := evaluateConstExprInModuleInstance(&d.OffsetExpression, m)
			if d.MemoryIndex >= uint32(len(m.Memories)) {
				return fmt.Errorf("%s[%d]: unknown memory %d", SectionIDName(SectionIDData), i, d.MemoryIndex)
			}
			offset := offsetExprResults[0]
			mem := m.Memories[d.MemoryIndex]
			if rangeOutOfBounds(offset, uint64(len(d.Init)), uint64(len(mem.Buffer))) {
				return fmt.Errorf("%s[%d]: out of bounds memory access", SectionIDName(SectionIDData), i)
			}
			copy(mem.Buffer[offset:], d.Init)
		}
	}
	return nil
}

// GetExport returns an export of the given name and type or errs if not exported or the wrong type.
func (m *ModuleInstance) getExport(name string, et ExternType) (*Export, error) {
	exp, ok := m.Exports[name]
	if !ok {
		return nil, fmt.Errorf("%q is not exported in module %q", name, m.ModuleName)
	}
	if exp.Type != et {
		return nil, fmt.Errorf("export %q in module %q is a %s, not a %s", name, m.ModuleName, ExternTypeName(exp.Type), ExternTypeName(et))
	}
	return exp, nil
}

func NewStore(enabledFeatures api.CoreFeatures, engine Engine) *Store {
	return &Store{
		nameToModule:       map[string]*ModuleInstance{},
		nameToModuleCap:    nameToModuleShrinkThreshold,
		EnabledFeatures:    enabledFeatures,
		Engine:             engine,
		typeIDs:            map[string]FunctionTypeID{},
		functionMaxTypes:   maximumFunctionTypes,
		MemoryLimitPages:   uint64(MemoryLimitPages),
		Memory64LimitPages: uint64(MemoryLimitPages),
	}
}

// MaxAllocatablePages is the most pages any memory can occupy on this platform,
// whatever the embedder configures with WithMemoryLimitPages or
// WithMemory64LimitPages: a slice length is an int, and make rejects a larger
// one by panicking rather than returning. Clamping here turns a module that asks
// for more into a clean instantiation error.
//
// It is applied at instantiation rather than at decode because the
// specification requires a module declaring limits no host could satisfy to
// still be *valid* -- see the memory64 section of RATIONALE.md. That covers the
// 32-bit case too: a four-gibibyte memory is longer than any slice where an int
// is 32 bits wide (GOARCH=386, arm, wasm), and this is what turns it away
// before make can panic.
const MaxAllocatablePages = uint64(math.MaxInt) >> MemoryPageSizeInBits

// memoryLimitPages returns the page ceiling that applies to mem.
func (s *Store) memoryLimitPages(mem *Memory) uint64 {
	limit := s.MemoryLimitPages
	if mem.IsMemory64 {
		limit = s.Memory64LimitPages
	}
	return min(limit, MaxAllocatablePages)
}

// maxMemoryLimitPages is the more permissive of the two ceilings, and bounds
// what one module may claim across all of its memories whatever index types it
// mixes.
func (s *Store) maxMemoryLimitPages() uint64 {
	return max(s.memoryLimitPages(&Memory{}), s.memoryLimitPages(&Memory{IsMemory64: true}))
}

// Instantiate uses name instead of the Module.NameSection ModuleName as it allows instantiating the same module under
// different names safely and concurrently.
//
// * ctx: the default context used for function calls.
// * name: the name of the module.
// * sys: the system context, which will be closed (SysContext.Close) on ModuleInstance.Close.
//
// Note: Module.Validate must be called prior to instantiation.
func (s *Store) Instantiate(
	ctx context.Context,
	module *Module,
	name string,
	sys *internalsys.Context,
	typeIDs []FunctionTypeID,
) (*ModuleInstance, error) {
	// Instantiate the module and add it to the store so that other modules can import it.
	m, err := s.instantiate(ctx, module, name, sys, typeIDs)
	if err != nil {
		return nil, err
	}

	// Now that the instantiation is complete without error, add it.
	if err = s.registerModule(m); err != nil {
		_ = m.Close(ctx)
		return nil, err
	}
	return m, nil
}

func (s *Store) instantiate(
	ctx context.Context,
	module *Module,
	name string,
	sysCtx *internalsys.Context,
	typeIDs []FunctionTypeID,
) (m *ModuleInstance, err error) {
	m = &ModuleInstance{ModuleName: name, TypeIDs: typeIDs, Sys: sysCtx, s: s, Source: module}

	m.Tables = make([]*TableInstance, int(module.ImportTableCount)+len(module.TableSection))
	m.Memories = make([]*MemoryInstance, int(module.ImportMemoryCount)+len(module.MemorySection))
	m.Globals = make([]*GlobalInstance, int(module.ImportGlobalCount)+len(module.GlobalSection))
	m.Tags = make([]*TagInstance, int(module.ImportTagCount)+len(module.TagSection))
	m.Engine, err = s.Engine.NewModuleEngine(module, m)
	if err != nil {
		return nil, err
	}

	if err = m.resolveImports(ctx, module); err != nil {
		return nil, err
	}

	err = m.buildTables(module,
		// As of reference-types proposal, boundary check must be done after instantiation.
		s.EnabledFeatures.IsEnabled(api.CoreFeatureReferenceTypes))
	if err != nil {
		return nil, err
	}

	allocator, _ := ctx.Value(expctxkeys.MemoryAllocatorKey{}).(api.MemoryAllocator)

	m.buildGlobals(module, m.Engine.FunctionInstanceReference)
	m.buildTags(module)
	if err = m.buildMemory(module, allocator, s); err != nil {
		return nil, err
	}
	m.Exports = module.Exports
	for _, exp := range m.Exports {
		if exp.Type == ExternTypeTable {
			t := m.Tables[exp.Index]
			t.involvingModuleInstances = append(t.involvingModuleInstances, m)
		}
	}

	// As of reference types proposal, data segment validation must happen after instantiation,
	// and the side effect must persist even if there's out of bounds error after instantiation.
	// https://github.com/WebAssembly/spec/blob/d39195773112a22b245ffbe864bab6d1182ccb06/test/core/linking.wast#L395-L405
	if !s.EnabledFeatures.IsEnabled(api.CoreFeatureReferenceTypes) {
		if err = m.validateData(module.DataSection); err != nil {
			return nil, err
		}
	}

	// After engine creation, we can create the funcref element instances and initialize funcref type globals.
	m.buildElementInstances(module.ElementSection)

	// Per https://webassembly.github.io/spec/core/exec/modules.html#exec-instantiation, active element
	// segments are applied before active data segments: an out-of-bounds trap partway through data
	// segment application must not undo element segment writes that already landed (see linking.wast's
	// "assert_trap ... out of bounds memory access" followed by an assert_return that observes the
	// same module's earlier element segment write survived).
	m.applyElements(module.ElementSection)

	// Now all the validation passes, we are safe to mutate memory instances (possibly imported ones).
	if err = m.applyData(module.DataSection); err != nil {
		return nil, err
	}

	m.Engine.DoneInstantiation()

	// Execute the start function.
	if module.StartSection != nil {
		funcIdx := *module.StartSection
		ce := m.Engine.NewFunction(funcIdx)
		_, err = ce.Call(ctx)
		if exitErr, ok := err.(*sys.ExitError); ok { // Don't wrap an exit error!
			return nil, exitErr
		} else if err != nil {
			return nil, fmt.Errorf("start %s failed: %w", module.funcDesc(SectionIDFunction, funcIdx), err)
		}
	}
	return
}

func (m *ModuleInstance) resolveImports(ctx context.Context, module *Module) (err error) {
	// Check if ctx contains an ImportResolver.
	resolveImport, _ := ctx.Value(expctxkeys.ImportResolverKey{}).(api.ImportResolver)

	for moduleName, imports := range module.ImportPerModule {
		var importedModule *ModuleInstance
		if resolveImport != nil {
			if v := resolveImport(moduleName); v != nil {
				switch mi := v.(type) {
				case *ModuleInstance:
					importedModule = mi
				case interface{ UnwrapModuleInstance() *ModuleInstance }:
					// A host module is a wrapper (wazy.hostModuleInstance) over its
					// *ModuleInstance; unwrap to the same object the store's own
					// module lookup would return for it. Lets the component graph
					// resolve a host module through the ImportResolver under a
					// stable key instead of its per-instantiation global name.
					importedModule = mi.UnwrapModuleInstance()
				default:
					return fmt.Errorf("import resolver returned an unsupported module type %T for %q", v, moduleName)
				}
			}
		}
		if importedModule == nil {
			importedModule, err = m.s.module(moduleName)
			if err != nil {
				return err
			}
		}

		for _, i := range imports {
			var imported *Export
			imported, err = importedModule.getExport(i.Name, i.Type)
			if err != nil {
				return
			}

			switch i.Type {
			case ExternTypeFunc:
				expectedType := &module.TypeSection[i.DescFunc]
				src := importedModule.Source
				actual := src.typeOfFunction(imported.Index)
				matched := false
				if m.TypeIDs != nil && importedModule.TypeIDs != nil {
					// Use structural type IDs for comparison (handles concrete ref types across modules).
					actualTypeIdx, ok := src.typeIndexOfFunction(imported.Index)
					matched = ok && importedModule.TypeIDs[actualTypeIdx] == m.TypeIDs[i.DescFunc]
				} else {
					matched = actual.EqualsSignature(expectedType.Params, expectedType.Results)
				}
				if !matched {
					err = errorInvalidImport(i, fmt.Errorf("signature mismatch: %s != %s", expectedType, actual))
					return
				}

				m.Engine.ResolveImportedFunction(i.IndexPerType, i.DescFunc, imported.Index, importedModule.Engine)
			case ExternTypeTable:
				expected := i.DescTable
				importedTable := importedModule.Tables[imported.Index]
				if expected.Type != importedTable.Type {
					err = errorInvalidImport(i, fmt.Errorf("table type mismatch: %s != %s",
						RefTypeName(expected.Type), RefTypeName(importedTable.Type)))
					return
				}

				if expected.IsTable64 != importedTable.IsTable64 {
					err = errorIndexTypeMismatch(i, expected.IsTable64, importedTable.IsTable64)
					return
				}

				if uint64(expected.Min) > uint64(len(importedTable.References)) {
					err = errorMinSizeMismatch(i, uint64(expected.Min), uint64(importedTable.Min))
					return
				}

				if expected.Max != nil {
					expectedMax := *expected.Max
					if importedTable.Max == nil {
						err = errorNoMax(i, uint64(expectedMax))
						return
					} else if expectedMax < *importedTable.Max {
						err = errorMaxSizeMismatch(i, uint64(expectedMax), uint64(*importedTable.Max))
						return
					}
				}
				m.Tables[i.IndexPerType] = importedTable
				importedTable.involvingModuleInstancesMutex.Lock()
				if len(importedTable.involvingModuleInstances) == 0 {
					panic("BUG: involvingModuleInstances must not be nil when it's imported")
				}
				importedTable.involvingModuleInstances = append(importedTable.involvingModuleInstances, m)
				importedTable.involvingModuleInstancesMutex.Unlock()
			case ExternTypeMemory:
				expected := i.DescMem
				importedMemory := importedModule.Memories[imported.Index]

				if expected.IsMemory64 != importedMemory.IsMemory64 {
					err = errorIndexTypeMismatch(i, expected.IsMemory64, importedMemory.IsMemory64)
					return
				}

				if expected.Min > importedMemory.Pages64() {
					err = errorMinSizeMismatch(i, expected.Min, importedMemory.Min)
					return
				}

				if expected.Max < importedMemory.Max {
					err = errorMaxSizeMismatch(i, expected.Max, importedMemory.Max)
					return
				}

				if expected.IsShared != importedMemory.Shared {
					err = errorSharedMismatch(i, expected.IsShared, importedMemory.Shared)
					return
				}

				// Mark this memory as shared before handing it out, so its
				// owner's Close (ensureResourcesClosed) can never decide to
				// recycle Buffer into the linear-memory buffer pool while we
				// (a second, independent ModuleInstance) hold a live
				// reference to it. Both sides serialize on mem.Mux, so
				// exactly one of "the owner already committed to closing" or
				// "we already registered as an importer" is observed here --
				// see memory_pool.go's doc for the full race argument.
				importedMemory.Mux.Lock()
				alreadyClosed := importedMemory.ownerClosed
				if !alreadyClosed {
					// Register as a live importer; this importer's own Close
					// decrements. Skipped when the owner already committed to
					// closing, since the import fails just below and this
					// ModuleInstance never Closes to balance the increment.
					importedMemory.importers++
					poolAuditHold(importedMemory, m)
				}
				importedMemory.Mux.Unlock()
				if alreadyClosed {
					err = errorInvalidImport(i, fmt.Errorf("memory owner module was closed concurrently"))
					return
				}

				m.Memories[i.IndexPerType] = importedMemory
				m.Engine.ResolveImportedMemory(i.IndexPerType, imported.Index, importedModule.Engine)
			case ExternTypeGlobal:
				expected := i.DescGlobal
				importedGlobal := importedModule.Globals[imported.Index]

				if expected.Mutable != importedGlobal.Type.Mutable {
					err = errorInvalidImport(i, fmt.Errorf("mutability mismatch: %t != %t",
						expected.Mutable, importedGlobal.Type.Mutable))
					return
				}

				if expected.Mutable && expected.ValType != importedGlobal.Type.ValType ||
					// nil type section: the two value types come from different modules, so a concrete ref's
					// index in one means nothing in the other. They can only match exactly, as before GC.
					!expected.Mutable && !isRefSubtypeOf(importedGlobal.Type.ValType, expected.ValType, nil) {
					err = errorInvalidImport(i, fmt.Errorf("value type mismatch: %s != %s",
						ValueTypeName(expected.ValType), ValueTypeName(importedGlobal.Type.ValType)))
					return
				}
				m.Globals[i.IndexPerType] = importedGlobal
			case ExternTypeTag:
				expected := &module.TypeSection[i.DescTag]
				importedTag := importedModule.Tags[imported.Index]
				if !importedTag.Type.EqualsType(expected) {
					err = errorInvalidImport(i, fmt.Errorf("tag type mismatch: %s != %s",
						expected, importedTag.Type))
					return
				}
				m.Tags[i.IndexPerType] = importedTag
			}
		}
	}
	return
}

func errorMinSizeMismatch(i *Import, expected, actual uint64) error {
	return errorInvalidImport(i, fmt.Errorf("minimum size mismatch: %d > %d", expected, actual))
}

func errorNoMax(i *Import, expected uint64) error {
	return errorInvalidImport(i, fmt.Errorf("maximum size mismatch: %d, but actual has no max", expected))
}

func errorMaxSizeMismatch(i *Import, expected, actual uint64) error {
	return errorInvalidImport(i, fmt.Errorf("maximum size mismatch: %d < %d", expected, actual))
}

func errorIndexTypeMismatch(i *Import, expected, actual bool) error {
	name := func(is64 bool) string {
		if is64 {
			return "i64"
		}
		return "i32"
	}
	return errorInvalidImport(i, fmt.Errorf("index type mismatch: expected %s, but actual has %s",
		name(expected), name(actual)))
}

func errorSharedMismatch(i *Import, expected, actual bool) error {
	return errorInvalidImport(i, fmt.Errorf("shared mismatch: expected %t, but actual has %t", expected, actual))
}

func errorInvalidImport(i *Import, err error) error {
	return fmt.Errorf("import %s[%s.%s]: %w", ExternTypeName(i.Type), i.Module, i.Name, err)
}

// initialize initializes the value of this global instance given the const expr and imported globals.
// funcRefResolver is called to get the actual funcref (engine specific) from the OpcodeRefFunc const expr.
//
// Global initialization constant expression can only reference the imported globals.
// See the note on https://www.w3.org/TR/2019/REC-wasm-core-1-20191205/#constant-expressions%E2%91%A0
func (g *GlobalInstance) initialize(importedGlobals []*GlobalInstance, expr *ConstantExpression, funcRefResolver func(funcIndex Index) Reference) {
	result, _, _ := evaluateConstExpr(
		expr,
		func(globalIndex Index) (ValueType, uint64, uint64, error) {
			g := importedGlobals[globalIndex]
			return g.Type.ValType, g.Val, g.ValHi, nil
		},
		func(funcIndex Index) (Reference, error) {
			return funcRefResolver(funcIndex), nil
		},
	)
	switch len(result) {
	case 1:
		g.Val = result[0]
	case 2:
		g.Val, g.ValHi = result[0], result[1]
	}
}

// String implements api.Global.
func (g *GlobalInstance) String() string {
	switch g.Type.ValType {
	case ValueTypeI32, ValueTypeI64:
		return fmt.Sprintf("global(%d)", g.Val)
	case ValueTypeF32:
		return fmt.Sprintf("global(%f)", api.DecodeF32(g.Val))
	case ValueTypeF64:
		return fmt.Sprintf("global(%f)", api.DecodeF64(g.Val))
	default:
		panic(fmt.Errorf("BUG: unknown value type %X", g.Type.ValType))
	}
}

func (g *GlobalInstance) Value() (uint64, uint64) {
	if g.Me != nil {
		return g.Me.GetGlobalValue(g.Index)
	}
	return g.Val, g.ValHi
}

func (g *GlobalInstance) SetValue(lo, hi uint64) {
	if g.Me != nil {
		g.Me.SetGlobalValue(g.Index, lo, hi)
	} else {
		g.Val, g.ValHi = lo, hi
	}
}

func (s *Store) GetFunctionTypeIDs(ts []FunctionType) ([]FunctionTypeID, error) {
	ret := make([]FunctionTypeID, len(ts))
	for i := range ts {
		t := &ts[i]
		key := structuralTypeKey(t, ret)
		id, err := s.getFunctionTypeIDByKey(key)
		if err != nil {
			return nil, err
		}
		ret[i] = id
	}
	return ret, nil
}

// structuralValueTypeName returns a string representation of a ValueType where
// concrete ref type indices are replaced with their FunctionTypeID. This makes
// the name independent of module-local type index numbering.
func structuralValueTypeName(vt ValueType, typeIDs []FunctionTypeID) string {
	if vt.IsConcreteRef() {
		idx := vt.TypeIndex()
		if !indexOutOfRange(idx, len(typeIDs)) {
			if vt.IsNullable() {
				return fmt.Sprintf("(ref null tid=%d)", typeIDs[idx])
			}
			return fmt.Sprintf("(ref tid=%d)", typeIDs[idx])
		}
	}
	return ValueTypeName(vt)
}

// structuralTypeKey returns a string key for a FunctionType that is stable
// across modules. For signatures without concrete ref types it falls back to
// FunctionType.key(). When concrete refs are present, local type indices are
// replaced with their already-assigned FunctionTypeID so that two modules
// defining structurally identical types at different indices produce the same
// key and share a single FunctionTypeID.
func structuralTypeKey(ft *FunctionType, typeIDs []FunctionTypeID) string {
	hasConcreteRef := false
	for _, p := range ft.Params {
		if p.IsConcreteRef() {
			hasConcreteRef = true
			break
		}
	}
	if !hasConcreteRef {
		for _, r := range ft.Results {
			if r.IsConcreteRef() {
				hasConcreteRef = true
				break
			}
		}
	}
	if !hasConcreteRef {
		return ft.key()
	}
	var ret string
	for _, b := range ft.Params {
		ret += structuralValueTypeName(b, typeIDs)
	}
	if len(ft.Params) == 0 {
		ret += "v_"
	} else {
		ret += "_"
	}
	for _, b := range ft.Results {
		ret += structuralValueTypeName(b, typeIDs)
	}
	if len(ft.Results) == 0 {
		ret += "v"
	}
	if ft.RecGroupSize > 1 {
		ret += fmt.Sprintf("|rec%d/%d", ft.RecGroupPosition, ft.RecGroupSize)
	}
	return ret
}

func (s *Store) GetFunctionTypeID(t *FunctionType) (FunctionTypeID, error) {
	return s.getFunctionTypeIDByKey(t.key())
}

func (s *Store) getFunctionTypeIDByKey(key string) (FunctionTypeID, error) {
	s.mux.RLock()
	id, ok := s.typeIDs[key]
	s.mux.RUnlock()
	if !ok {
		s.mux.Lock()
		defer s.mux.Unlock()
		// Check again in case another goroutine has already added the type.
		if id, ok = s.typeIDs[key]; ok {
			return id, nil
		}
		l := len(s.typeIDs)
		if uint32(l) >= s.functionMaxTypes {
			return 0, fmt.Errorf("too many function types in a store")
		}
		id = FunctionTypeID(l)
		s.typeIDs[key] = id
	}
	return id, nil
}

// CloseWithExitCode implements the same method as documented on wazy.Runtime.
func (s *Store) CloseWithExitCode(ctx context.Context, exitCode uint32) error {
	s.mux.Lock()
	defer s.mux.Unlock()
	// Close modules in reverse initialization order.
	var errs []error
	for m := s.moduleList; m != nil; m = m.next {
		// If closing this module errs, proceed anyway to close the others.
		if err := m.closeWithExitCode(ctx, exitCode); err != nil {
			errs = append(errs, err)
		}
	}
	s.moduleList = nil
	s.nameToModule = nil
	s.nameToModuleCap = 0
	s.typeIDs = nil
	return errors.Join(errs...)
}
