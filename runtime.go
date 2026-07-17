package wazy

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/samyfodil/wazy/api"
	experimentalapi "github.com/samyfodil/wazy/experimental"
	"github.com/samyfodil/wazy/internal/engine/interpreter"
	"github.com/samyfodil/wazy/internal/engine/native"
	"github.com/samyfodil/wazy/internal/expctxkeys"
	"github.com/samyfodil/wazy/internal/platform"
	internalsock "github.com/samyfodil/wazy/internal/sock"
	internalsys "github.com/samyfodil/wazy/internal/sys"
	"github.com/samyfodil/wazy/internal/wasm"
	binaryformat "github.com/samyfodil/wazy/internal/wasm/binary"
	"github.com/samyfodil/wazy/sys"
)

// Runtime allows embedding of WebAssembly modules.
//
// The below is an example of basic initialization:
//
//	ctx := context.Background()
//	r := wazy.NewRuntime(ctx)
//	defer r.Close(ctx) // This closes everything this Runtime created.
//
//	mod, _ := r.Instantiate(ctx, wasm)
//
// # Notes
//
//   - This is an interface for decoupling, not third-party implementations.
//     All implementations are in wazy.
//   - Closing this closes any CompiledModule or Module it instantiated.
type Runtime interface {
	// Instantiate instantiates a module from the WebAssembly binary (%.wasm)
	// with default configuration, which notably calls the "_start" function,
	// if it exists.
	//
	// Here's an example:
	//	ctx := context.Background()
	//	r := wazy.NewRuntime(ctx)
	//	defer r.Close(ctx) // This closes everything this Runtime created.
	//
	//	mod, _ := r.Instantiate(ctx, wasm)
	//
	// # Notes
	//
	//   - See notes on InstantiateModule for error scenarios.
	//   - See InstantiateWithConfig for configuration overrides.
	Instantiate(ctx context.Context, source []byte) (api.Module, error)

	// InstantiateWithConfig instantiates a module from the WebAssembly binary
	// (%.wasm) or errs for reasons including exit or validation.
	//
	// Here's an example:
	//	ctx := context.Background()
	//	r := wazy.NewRuntime(ctx)
	//	defer r.Close(ctx) // This closes everything this Runtime created.
	//
	//	mod, _ := r.InstantiateWithConfig(ctx, wasm,
	//		wazy.NewModuleConfig().WithName("rotate"))
	//
	// # Notes
	//
	//   - See notes on InstantiateModule for error scenarios.
	//   - If you aren't overriding defaults, use Instantiate.
	//   - This is a convenience utility that chains CompileModule with
	//     InstantiateModule. To instantiate the same source multiple times,
	//     use CompileModule as InstantiateModule avoids redundant decoding
	//     and/or compilation.
	InstantiateWithConfig(ctx context.Context, source []byte, config ModuleConfig) (api.Module, error)

	// NewHostModuleBuilder lets you create modules out of functions defined in Go.
	//
	// Below defines and instantiates a module named "env" with one function:
	//
	//	ctx := context.Background()
	//	hello := func(context.Context, api.Module) {
	//		fmt.Fprintln(stdout, "hello!")
	//	}
	//	_, err := wazy.HostProc0(r.NewHostModuleBuilder("env").NewFunctionBuilder(), hello).
	//		Export("hello").
	//		Instantiate(ctx, r)
	//
	// Note: empty `moduleName` is not allowed.
	NewHostModuleBuilder(moduleName string) HostModuleBuilder

	// CompileModule decodes the WebAssembly binary (%.wasm) or errs if invalid.
	// Any pre-compilation done after decoding wasm is dependent on RuntimeConfig.
	//
	// There are two main reasons to use CompileModule instead of Instantiate:
	//   - Improve performance when the same module is instantiated multiple times under different names
	//   - Reduce the amount of errors that can occur during InstantiateModule.
	//
	// # Notes
	//
	//   - The resulting module name defaults to what was binary from the custom name section.
	//   - Any pre-compilation done after decoding the source is dependent on RuntimeConfig.
	//
	// See https://www.w3.org/TR/2019/REC-wasm-core-1-20191205/#name-section%E2%91%A0
	CompileModule(ctx context.Context, binary []byte) (CompiledModule, error)

	// InstantiateModule instantiates the module or errs for reasons including
	// exit or validation.
	//
	// Here's an example:
	//	mod, _ := n.InstantiateModule(ctx, compiled, wazy.NewModuleConfig().
	//		WithName("prod"))
	//
	// # Errors
	//
	// While CompiledModule is pre-validated, there are a few situations which
	// can cause an error:
	//   - The module name is already in use.
	//   - The module has a table element initializer that resolves to an index
	//     outside the Table minimum size.
	//   - The module has a start function, and it failed to execute.
	//   - The module was compiled to WASI and exited with a non-zero exit
	//     code, you'll receive a sys.ExitError.
	//   - RuntimeConfig.WithCloseOnContextDone was enabled and a context
	//     cancellation or deadline triggered before a start function returned.
	InstantiateModule(ctx context.Context, compiled CompiledModule, config ModuleConfig) (api.Module, error)

	// CloseWithExitCode closes all the modules that have been initialized in this Runtime with the provided exit code.
	// An error is returned if any module returns an error when closed.
	//
	// Here's an example:
	//	ctx := context.Background()
	//	r := wazy.NewRuntime(ctx)
	//	defer r.CloseWithExitCode(ctx, 2) // This closes everything this Runtime created.
	//
	//	// Everything below here can be closed, but will anyway due to above.
	//	_, _ = wasi_snapshot_preview1.InstantiateSnapshotPreview1(ctx, r)
	//	mod, _ := r.Instantiate(ctx, wasm)
	CloseWithExitCode(ctx context.Context, exitCode uint32) error

	// Module returns an instantiated module in this runtime or nil if there aren't any.
	Module(moduleName string) api.Module

	// Closer closes all compiled code by delegating to CloseWithExitCode with an exit code of zero.
	api.Closer
}

// NewRuntime returns a runtime with a configuration assigned by NewRuntimeConfig.
func NewRuntime(ctx context.Context) Runtime {
	return NewRuntimeWithConfig(ctx, NewRuntimeConfig())
}

// NewRuntimeWithConfig returns a runtime with the given configuration.
func NewRuntimeWithConfig(ctx context.Context, rConfig RuntimeConfig) Runtime {
	config := rConfig.(*runtimeConfig)
	configKind := config.engineKind
	configEngine := config.newEngine
	if configKind == engineKindAuto {
		if platform.CompilerSupports(config.enabledFeatures) {
			configKind = engineKindCompiler
		} else {
			configKind = engineKindInterpreter
		}
	}
	if configEngine == nil {
		if configKind == engineKindCompiler {
			configEngine = native.NewEngine
		} else {
			configEngine = interpreter.NewEngine
		}
	}
	var engine wasm.Engine
	var cacheImpl *cache
	if c := config.cache; c != nil {
		// If the Cache is configured, we share the engine.
		cacheImpl = c.(*cache)
		engine = cacheImpl.initEngine(configKind, configEngine, ctx, config.enabledFeatures)
	} else {
		// Otherwise, we create a new engine.
		engine = configEngine(ctx, config.enabledFeatures, nil)
	}
	store := wasm.NewStore(config.enabledFeatures, engine)
	return &runtime{
		cache:                 cacheImpl,
		store:                 store,
		enabledFeatures:       config.enabledFeatures,
		memoryLimitPages:      config.memoryLimitPages,
		memoryCapacityFromMax: config.memoryCapacityFromMax,
		dwarfDisabled:         config.dwarfDisabled,
		storeCustomSections:   config.storeCustomSections,
		ensureTermination:     config.ensureTermination,
	}
}

// runtime allows decoupling of public interfaces from internal representation.
type runtime struct {
	store                 *wasm.Store
	cache                 *cache
	enabledFeatures       api.CoreFeatures
	memoryLimitPages      uint32
	memoryCapacityFromMax bool
	dwarfDisabled         bool
	storeCustomSections   bool

	// closed is the pointer used both to guard moduleEngine.CloseWithExitCode and to store the exit code.
	//
	// The update value is 1 + exitCode << 32. This ensures an exit code of zero isn't mistaken for never closed.
	//
	// Note: Exclusively reading and updating this with atomics guarantees cross-goroutine observations.
	// See /RATIONALE.md
	closed atomic.Uint64

	ensureTermination bool
}

// Module implements Runtime.Module.
func (r *runtime) Module(moduleName string) api.Module {
	if len(moduleName) == 0 {
		return nil
	}
	m := r.store.Module(moduleName)
	if m == nil {
		return nil
	} else if m.Source.IsHostModule {
		return hostModuleInstance{m}
	}
	return m
}

// DecodeModuleNoCompile decodes a core module's structure (type/import/etc.
// sections) using this runtime's exact decode settings, WITHOUT native codegen
// or validation. The component instantiate path uses it to read an embedded
// core module's import signatures cheaply, instead of throwaway-compiling the
// module a second time just to learn them (the real compile happens once, in
// the instantiation loop). Not part of the Runtime interface -- reached via a
// type assertion from internal/component/instance.
//
// dwarf/custom sections are skipped (unneeded for import discovery), and the
// memory limit is the absolute max so a large declared memory never wrongly
// fails discovery -- the real compile still enforces this runtime's true limit.
func (r *runtime) DecodeModuleNoCompile(bin []byte) (*wasm.Module, error) {
	return binaryformat.DecodeModule(bin, r.enabledFeatures, wasm.MemoryLimitPages, false, false, false)
}

// CompileModule implements Runtime.CompileModule
func (r *runtime) CompileModule(ctx context.Context, binary []byte) (CompiledModule, error) {
	if err := r.failIfClosed(); err != nil {
		return nil, err
	}

	internal, err := binaryformat.DecodeModule(binary, r.enabledFeatures,
		r.memoryLimitPages, r.memoryCapacityFromMax, !r.dwarfDisabled, r.storeCustomSections)
	if err != nil {
		return nil, err
	}

	// The loop interrupt-check interval (for WithCloseOnContextDone) is a
	// per-compile setting read from ctx; it is folded into the module ID so
	// distinct intervals compile to distinct cached variants. The engine reads
	// the same value from ctx to configure loop lowering.
	interruptCheckInterval := wasm.InterruptCheckIntervalFromContext(ctx)
	if err = wasm.ValidateInterruptCheckInterval(interruptCheckInterval); err != nil {
		return nil, err
	}

	// The module ID is a hash over the raw binary plus the compile-affecting
	// flags (listener presence per function, ensureTermination). Computing it
	// before validating lets us ask the engine whether it already has a
	// compiled artifact for this exact binary and skip the (expensive)
	// function-body validation pass below when it does.
	//
	// buildFunctionListeners, however, is NOT safe to run on a decoded but
	// not-yet-validated module: it builds FunctionDefinition metadata that
	// indexes TypeSection by each function's / imported function's declared
	// type index, and those indices are only checked by validation (the
	// per-function-body pass rejects an out-of-range one). Building them
	// first would panic on an adversarial/corrupt binary that decodes but
	// fails validation. So we only take the probe-before-validate fast path
	// when there is no FunctionListenerFactory in ctx - the overwhelmingly
	// common case, in which listeners is nil and nothing indexes TypeSection
	// ahead of validation. With a factory present (an experimental,
	// non-hot-path feature) we keep the original validate-first ordering and
	// forgo the cache-hit validation skip; the engine's own CompileModule
	// still resolves a cached artifact cheaply on its internal probe.
	var listeners []experimentalapi.FunctionListener
	var hasCompiled bool
	if ctx.Value(expctxkeys.FunctionListenerFactoryKey{}) == nil {
		internal.AssignModuleID(binary, nil, r.ensureTermination, interruptCheckInterval)

		// hasCompiled acquires a reference on the engine's cached artifact
		// for this module.ID (if any) - see wasm.Engine.HasCompiledModule. We
		// must balance that reference: either reach the CompiledModule return
		// below (whose Close releases it), or explicitly release it on any
		// error returned between here and there.
		if hasCompiled, err = r.store.Engine.HasCompiledModule(internal, nil, r.ensureTermination); err != nil {
			return nil, err
		}

		if hasCompiled {
			// TRUST MODEL: a hit here can come from the engine's on-disk file
			// cache (native), which we already trust no less than a fresh
			// compile - we mmap and EXECUTE the machine code either way. The
			// crc32 checksum embedded in the cache entry still guards against
			// corruption, and a wazyVersion mismatch still forces a recompile;
			// neither of those checks is weakened here. Given that, skipping
			// *revalidation* of the source binary adds no new trust: a hit is
			// only possible when module.ID - a sha256 over the raw binary plus
			// every compile-affecting flag (see AssignModuleID: listener
			// presence per function, ensureTermination) - matches exactly,
			// which can only happen if this exact binary, with these exact
			// flags, was already compiled (and therefore already ran the
			// per-function-body opcode walk successfully) before.
			//
			// We still run ValidateStructure: the cheap, non-per-function-body
			// checks it performs (e.g. caching FunctionType.ParamNumInUint64 /
			// ResultNumInUint64) populate fields on *this* freshly decoded
			// Module value, which the engine and instantiation read directly -
			// they are not carried over from the cached artifact.
			if err = internal.ValidateStructure(r.enabledFeatures); err != nil {
				r.store.Engine.DeleteCompiledModule(internal)
				return nil, err
			}
		} else if err = internal.Validate(r.enabledFeatures); err != nil {
			// TODO: decoders should validate before returning, as that allows
			// them to err with the correct position in the wasm binary.
			return nil, err
		}
	} else {
		if err = internal.Validate(r.enabledFeatures); err != nil {
			// TODO: decoders should validate before returning, as that allows
			// them to err with the correct position in the wasm binary.
			return nil, err
		}
		if listeners, err = buildFunctionListeners(ctx, internal); err != nil {
			return nil, err
		}
		internal.AssignModuleID(binary, listeners, r.ensureTermination, interruptCheckInterval)
	}

	// Now that the module is validated, cache the memory definitions.
	// TODO: lazy initialization of memory definition.
	internal.BuildMemoryDefinitions()

	c := &compiledModule{module: internal, compiledEngine: r.store.Engine}

	// typeIDs are static and compile-time known.
	typeIDs, err := r.store.GetFunctionTypeIDs(internal.TypeSection)
	if err != nil {
		if hasCompiled {
			r.store.Engine.DeleteCompiledModule(internal)
		}
		return nil, err
	}
	c.typeIDs = typeIDs

	if !hasCompiled {
		// On a miss, CompileModule performs its own (single) probe of memory
		// and file cache before compiling; we don't duplicate that here to
		// avoid racing a concurrent compile of the same module twice.
		if err = r.store.Engine.CompileModule(ctx, internal, listeners, r.ensureTermination); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func buildFunctionListeners(ctx context.Context, internal *wasm.Module) ([]experimentalapi.FunctionListener, error) {
	// Test to see if internal code are using an experimental feature.
	fnlf := ctx.Value(expctxkeys.FunctionListenerFactoryKey{})
	if fnlf == nil {
		return nil, nil
	}
	factory := fnlf.(experimentalapi.FunctionListenerFactory)
	importCount := internal.ImportFunctionCount
	listeners := make([]experimentalapi.FunctionListener, len(internal.FunctionSection))
	for i := 0; i < len(listeners); i++ {
		listeners[i] = factory.NewFunctionListener(internal.FunctionDefinition(uint32(i) + importCount))
	}
	return listeners, nil
}

// failIfClosed returns an error if CloseWithExitCode was called implicitly (by Close) or explicitly.
func (r *runtime) failIfClosed() error {
	if closed := r.closed.Load(); closed != 0 {
		return fmt.Errorf("runtime closed with exit_code(%d)", uint32(closed>>32))
	}
	return nil
}

// Instantiate implements Runtime.Instantiate
func (r *runtime) Instantiate(ctx context.Context, binary []byte) (api.Module, error) {
	return r.InstantiateWithConfig(ctx, binary, NewModuleConfig())
}

// InstantiateWithConfig implements Runtime.InstantiateWithConfig
func (r *runtime) InstantiateWithConfig(ctx context.Context, binary []byte, config ModuleConfig) (api.Module, error) {
	if compiled, err := r.CompileModule(ctx, binary); err != nil {
		return nil, err
	} else {
		compiled.(*compiledModule).closeWithModule = true
		return r.InstantiateModule(ctx, compiled, config)
	}
}

// InstantiateModule implements Runtime.InstantiateModule.
func (r *runtime) InstantiateModule(
	ctx context.Context,
	compiled CompiledModule,
	mConfig ModuleConfig,
) (mod api.Module, err error) {
	if err = r.failIfClosed(); err != nil {
		return nil, err
	}

	code := compiled.(*compiledModule)
	config := mConfig.(*moduleConfig)

	// Only add guest module configuration to guests.
	if !code.module.IsHostModule {
		if sockConfig, ok := ctx.Value(internalsock.ConfigKey{}).(*internalsock.Config); ok {
			// ModuleConfig is documented immutable and may be reused across
			// (concurrent) instantiations, so copy before setting the per-ctx
			// socket config rather than mutating the caller's value in place
			// (a data race, and a leak of one ctx's sockConfig into later
			// instantiations sharing the config). A shallow struct copy
			// suffices: only the sockConfig pointer changes; every other field
			// (including the shared map refs) is read-only from here on.
			cfg := *config
			cfg.sockConfig = sockConfig
			config = &cfg
		}
	}

	// Host modules never consult their own Sys: host functions are invoked with the
	// CALLING module's Sys (see wasi_snapshot_preview1 and assemblyscript imports, which
	// all read mod.(*wasm.ModuleInstance).Sys off the module passed to the host function,
	// not off themselves). Building one here would be pure waste (rand source, fake clocks,
	// etc.) for something that's never read; ModuleInstance.Sys == nil is already the
	// documented expectation for such modules (see the "nil if from HostModuleBuilder"
	// comment in internal/wasm/module_instance.go's ensureResourcesClosed).
	var sysCtx *internalsys.Context
	if !code.module.IsHostModule {
		if sysCtx, err = config.toSysContext(); err != nil {
			return nil, err
		}
	}

	name := config.name
	if !config.nameSet && code.module.NameSection != nil && code.module.NameSection.ModuleName != "" {
		name = code.module.NameSection.ModuleName
	}

	// Instantiate the module.
	mod, err = r.store.Instantiate(ctx, code.module, name, sysCtx, code.typeIDs)
	if err != nil {
		// If there was an error, don't leak the compiled module.
		if code.closeWithModule {
			_ = code.Close(ctx) // don't overwrite the error
		}
		return nil, err
	}

	if closeNotifier, ok := ctx.Value(expctxkeys.CloseNotifierKey{}).(experimentalapi.CloseNotifier); ok {
		mod.(*wasm.ModuleInstance).CloseNotifier = closeNotifier
	}

	// Attach the code closer so that anything afterward closes the compiled
	// code when closing the module.
	if code.closeWithModule {
		mod.(*wasm.ModuleInstance).CodeCloser = code
	}

	// Now, invoke any start functions, failing at first error.
	for _, fn := range config.startFunctions {
		start := mod.ExportedFunction(fn)
		if start == nil {
			continue
		}
		if _, err = start.Call(ctx); err != nil {
			_ = mod.Close(ctx) // Don't leak the module on error.

			if se, ok := err.(*sys.ExitError); ok {
				if se.ExitCode() == 0 { // Don't err on success.
					err = nil
				}
				return // Don't wrap an exit error
			}
			err = fmt.Errorf("module[%s] function[%s] failed: %w", name, fn, err)
			return
		}
	}
	return
}

// Close implements api.Closer embedded in Runtime.
func (r *runtime) Close(ctx context.Context) error {
	return r.CloseWithExitCode(ctx, 0)
}

// CloseWithExitCode implements Runtime.CloseWithExitCode
//
// Note: it also marks the internal `closed` field
func (r *runtime) CloseWithExitCode(ctx context.Context, exitCode uint32) error {
	closed := uint64(1) + uint64(exitCode)<<32 // Store exitCode as high-order bits.
	if !r.closed.CompareAndSwap(0, closed) {
		return nil
	}
	err := r.store.CloseWithExitCode(ctx, exitCode)
	if r.cache == nil {
		// Close the engine if the cache is not configured, which means that this engine is scoped in this runtime.
		if errCloseEngine := r.store.Engine.Close(); errCloseEngine != nil {
			return errCloseEngine
		}
	}
	return err
}
