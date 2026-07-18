package wasm

import (
	"context"

	"github.com/samyfodil/wazy/api"
)

// Engine is a Store-scoped mechanism to compile functions declared or imported by a module.
// This is a top-level type implemented by an interpreter or compiler.
type Engine interface {
	// Close closes this engine, and releases all the compiled cache.
	Close() (err error)

	// CompileModule implements the same method as documented on wasm.Engine.
	CompileModule(ctx context.Context, module *Module, listeners []api.FunctionListener, ensureTermination bool) error

	// HasCompiledModule reports whether this engine already holds a compiled
	// artifact for module (identified by module.ID - see Module.AssignModuleID),
	// either resident in memory or found in on-disk file cache. A true result
	// acquires a reference on the artifact exactly as CompileModule's own
	// cache-hit path would (bumping an internal refcount, and for a file-cache
	// hit, promoting it into the in-memory cache as a side effect so a
	// following CompileModule call for the same module resolves via the cheap
	// in-memory path instead of re-reading the file cache).
	//
	// A true result acquires exactly ONE reference, and the caller MUST
	// release exactly one to balance it. The reference is meant to be handed
	// to the resulting CompiledModule so its Close releases it; on any error
	// path that prevents returning that CompiledModule, the caller must
	// instead call DeleteCompiledModule once. Failing to release leaks the
	// reference and the module is never released from cache tracking.
	//
	// Because a true result already holds a reference, the caller must NOT
	// additionally call CompileModule for the same module while holding it:
	// CompileModule's own cache-hit path acquires a SECOND reference (it is
	// not idempotent on the refcount), which would then be unbalanced. On a
	// hit, skip CompileModule entirely; only call it on a false result.
	//
	// module.ID must already be assigned (via Module.AssignModuleID) before
	// calling this. listeners and ensureTermination must be the same values
	// that will be passed to CompileModule for this module.
	//
	// This exists so wazy.Runtime.CompileModule can skip the expensive
	// per-function-body validation pass on a cache hit: see the TRUST MODEL
	// note at that call site.
	HasCompiledModule(module *Module, listeners []api.FunctionListener, ensureTermination bool) (bool, error)

	// CompiledModuleCount is exported for testing, to track the size of the compilation cache.
	CompiledModuleCount() uint32

	// DeleteCompiledModule releases compilation caches for the given module (source).
	// Note: it is safe to call this function for a module from which module instances are instantiated even when these
	// module instances have outstanding calls.
	DeleteCompiledModule(module *Module)

	// NewModuleEngine compiles down the function instances in a module, and returns ModuleEngine for the module.
	//
	// * module is the source module from which moduleFunctions are instantiated. This is used for caching.
	// * instance is the *ModuleInstance which is created from `module`.
	//
	// Note: Input parameters must be pre-validated with wasm.Module Validate, to ensure no fields are invalid
	// due to reasons such as out-of-bounds.
	NewModuleEngine(module *Module, instance *ModuleInstance) (ModuleEngine, error)
}

// ModuleEngine implements function calls for a given module.
type ModuleEngine interface {
	// DoneInstantiation is called at the end of the instantiation of the module.
	DoneInstantiation()

	// NewFunction returns an api.Function for the given function pointed by the given Index.
	NewFunction(index Index) api.Function

	// ResolveImportedFunction is used to add imported functions needed to make this ModuleEngine fully functional.
	// 	- `index` is the function Index of this imported function.
	// 	- `descFunc` is the type Index in Module.TypeSection of this imported function. It corresponds to Import.DescFunc.
	// 	- `indexInImportedModule` is the function Index of the imported function in the imported module.
	//	- `importedModuleEngine` is the ModuleEngine for the imported ModuleInstance.
	ResolveImportedFunction(index, descFunc, indexInImportedModule Index, importedModuleEngine ModuleEngine)

	// ResolveImportedMemory is called when this module imports a memory from another module.
	ResolveImportedMemory(importedModuleEngine ModuleEngine)

	// LookupFunction returns the FunctionModule and the Index of the function in the returned ModuleInstance at the given offset in the table.
	LookupFunction(t *TableInstance, typeId FunctionTypeID, tableOffset Index) (*ModuleInstance, Index)

	// GetGlobalValue returns the value of the global variable at the given Index.
	// Only called when OwnsGlobals() returns true, and must not be called for imported globals
	GetGlobalValue(idx Index) (lo, hi uint64)

	// SetGlobalValue sets the value of the global variable at the given Index.
	// Only called when OwnsGlobals() returns true, and must not be called for imported globals
	SetGlobalValue(idx Index, lo, hi uint64)

	// OwnsGlobals returns true if this ModuleEngine owns the global variables. If true, wasm.GlobalInstance's Val,ValHi should
	// not be accessed directly.
	OwnsGlobals() bool

	// FunctionInstanceReference returns Reference for the given Index for a FunctionInstance. The returned values are used by
	// the initialization via ElementSegment.
	FunctionInstanceReference(funcIndex Index) Reference

	// MemoryGrown notifies the engine that the memory has grown.
	MemoryGrown()
}
