package wazy

import (
	"context"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/wasm"
)

// HostFunctionBuilder defines a host function (in Go), so that a
// WebAssembly binary (e.g. %.wasm file) can import and use it.
//
// There are two ways to define a host function's implementation and its
// WebAssembly signature:
//
//   - The strongly-typed HostFunc0-HostFunc8 and HostProc0-HostProc8 helpers
//     (in the root wazy package) derive the WebAssembly signature from a Go
//     function whose parameters and result are numeric (a HostValue),
//     prefixed by context.Context and api.Module. They are chainable:
//
//     wazy.HostFunc2(b, func(ctx context.Context, mod api.Module, x, y uint32) uint32 {
//     return x + y
//     }).Export("add")
//
//   - WithGoModuleFunction (and WithGoFunction, when the calling module is not
//     needed) take an explicit WebAssembly signature and read parameters from,
//     and write results to, the raw stack. Use these for signatures the typed
//     helpers cannot express: more than 8 parameters, multiple results, or no
//     context.Context/api.Module prefix.
//
// # Memory
//
// All host functions act on the importing api.Module, including any memory
// exported in its binary (%.wasm file). If you are reading or writing memory,
// it is sand-boxed Wasm memory defined by the guest.
//
// Below, `m` is the importing module, defined in Wasm. `fn` is a host function
// added via Export. This means that `x` was read from memory defined in Wasm,
// not arbitrary memory in the process.
//
//	fn := func(ctx context.Context, m api.Module, offset uint32) uint32 {
//		x, _ := m.Memory().ReadUint32Le(ctx, offset)
//		return x
//	}
//
// # Notes
//
//   - This is an interface for decoupling, not third-party implementations.
//     All implementations are in wazy.
type HostFunctionBuilder interface {
	// WithGoFunction registers a host function from an explicit WebAssembly
	// signature and an api.GoFunction that reads parameters from, and writes
	// results to, the raw stack. It is the lower-level counterpart to the
	// typed HostFunc0-HostFunc8/HostProc0-HostProc8 helpers, and is the right
	// tool when their fixed "numeric, ctx+mod prefix" shape does not fit.
	//
	// Here's an example addition function:
	//
	//	builder.WithGoFunction(api.GoFunc(func(ctx context.Context, stack []uint64) {
	//		x, y := api.DecodeI32(stack[0]), api.DecodeI32(stack[1])
	//		sum := x + y
	//		stack[0] = api.EncodeI32(sum)
	//	}), []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32})
	//
	// As you can see above, defining in this way implies knowledge of which
	// WebAssembly api.ValueType is appropriate for each parameter and result.
	//
	// See WithGoModuleFunction if you also need to access the calling module.
	WithGoFunction(fn api.GoFunction, params, results []api.ValueType) HostFunctionBuilder

	// WithGoModuleFunction registers a host function from an explicit
	// WebAssembly signature and an api.GoModuleFunction that reads parameters
	// from, and writes results to, the raw stack. It is the lower-level
	// counterpart to the typed HostFunc0-HostFunc8/HostProc0-HostProc8
	// helpers, and is the right tool when their fixed "numeric, ctx+mod
	// prefix" shape does not fit.
	//
	// Here's an example addition function that loads operands from memory:
	//
	//	builder.WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, m api.Module, stack []uint64) {
	//		mem := m.Memory()
	//		offset := api.DecodeU32(stack[0])
	//
	//		x, _ := mem.ReadUint32Le(ctx, offset)
	//		y, _ := mem.ReadUint32Le(ctx, offset + 4) // 32 bits == 4 bytes!
	//		sum := x + y
	//
	//		stack[0] = api.EncodeU32(sum)
	//	}), []api.ValueType{api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32})
	//
	// As you can see above, defining in this way implies knowledge of which
	// WebAssembly api.ValueType is appropriate for each parameter and result.
	//
	// See WithGoFunction if you don't need access to the calling module.
	WithGoModuleFunction(fn api.GoModuleFunction, params, results []api.ValueType) HostFunctionBuilder

	// WithName defines the optional module-local name of this function, e.g.
	// "random_get"
	//
	// Note: This is not required to match the Export name.
	WithName(name string) HostFunctionBuilder

	// WithParameterNames defines optional parameter names of the function
	// signature, e.x. "buf", "buf_len"
	//
	// Note: When defined, names must be provided for all parameters.
	WithParameterNames(names ...string) HostFunctionBuilder

	// WithResultNames defines optional result names of the function
	// signature, e.x. "errno"
	//
	// Note: When defined, names must be provided for all results.
	WithResultNames(names ...string) HostFunctionBuilder

	// Export exports this to the HostModuleBuilder as the given name, e.g.
	// "random_get"
	Export(name string) HostModuleBuilder
}

// HostModuleBuilder is a way to define host functions (in Go), so that a
// WebAssembly binary (e.g. %.wasm file) can import and use them.
//
// Specifically, this implements the host side of an Application Binary
// Interface (ABI) like WASI or AssemblyScript.
//
// For example, this defines and instantiates a module named "env" with one
// function:
//
//	ctx := context.Background()
//	r := wazy.NewRuntime(ctx)
//	defer r.Close(ctx) // This closes everything this Runtime created.
//
//	hello := func(context.Context, api.Module) {
//		println("hello!")
//	}
//	env, _ := wazy.HostProc0(r.NewHostModuleBuilder("env").NewFunctionBuilder(), hello).
//		Export("hello").
//		Instantiate(ctx)
//
// If the same module may be instantiated multiple times, it is more efficient
// to separate steps. Here's an example using WithGoModuleFunction for a
// function whose signature the typed helpers don't cover:
//
//	compiled, _ := r.NewHostModuleBuilder("env").
//		NewFunctionBuilder().
//		WithGoModuleFunction(getRandomString, nil, []api.ValueType{api.ValueTypeI32}).
//		Export("get_random_string").
//		Compile(ctx)
//
//	env1, _ := r.InstantiateModule(ctx, compiled, wazy.NewModuleConfig().WithName("env.1"))
//	env2, _ := r.InstantiateModule(ctx, compiled, wazy.NewModuleConfig().WithName("env.2"))
//
// See HostFunctionBuilder for valid host function signatures and other details.
//
// # Notes
//
//   - This is an interface for decoupling, not third-party implementations.
//     All implementations are in wazy.
//   - HostModuleBuilder is mutable: each method returns the same instance for
//     chaining.
//   - methods do not return errors, to allow chaining. Any validation errors
//     are deferred until Compile.
//   - Functions are indexed in order of calls to NewFunctionBuilder as
//     insertion ordering is needed by ABI such as Emscripten (invoke_*).
//   - The semantics of host functions assumes the existence of an "importing module" because, for example, the host function needs access to
//     the memory of the importing module. Therefore, direct use of ExportedFunction is forbidden for host modules.
//     Practically speaking, it is usually meaningless to directly call a host function from Go code as it is already somewhere in Go code.
type HostModuleBuilder interface {
	// Note: until golang/go#5860, we can't use example tests to embed code in interface godocs.

	// NewFunctionBuilder begins the definition of a host function.
	NewFunctionBuilder() HostFunctionBuilder

	// Compile returns a CompiledModule that can be instantiated by Runtime.
	Compile(context.Context) (CompiledModule, error)

	// Instantiate is a convenience that calls Compile, then Runtime.InstantiateModule.
	// This can fail for reasons documented on Runtime.InstantiateModule.
	//
	// Here's an example:
	//
	//	ctx := context.Background()
	//	r := wazy.NewRuntime(ctx)
	//	defer r.Close(ctx) // This closes everything this Runtime created.
	//
	//	hello := func(context.Context, api.Module) {
	//		println("hello!")
	//	}
	//	env, _ := wazy.HostProc0(r.NewHostModuleBuilder("env").NewFunctionBuilder(), hello).
	//		Export("hello").
	//		Instantiate(ctx)
	//
	// # Notes
	//
	//   - Closing the Runtime has the same effect as closing the result.
	//   - Fields in the builder are copied during instantiation: Later changes do not affect the instantiated result.
	//   - To avoid using configuration defaults, use Compile instead.
	Instantiate(context.Context) (api.Module, error)
}

// hostModuleBuilder implements HostModuleBuilder
type hostModuleBuilder struct {
	r              *runtime
	moduleName     string
	exportNames    []string
	nameToHostFunc map[string]*wasm.HostFunc
}

// NewHostModuleBuilder implements Runtime.NewHostModuleBuilder
func (r *runtime) NewHostModuleBuilder(moduleName string) HostModuleBuilder {
	return &hostModuleBuilder{
		r:              r,
		moduleName:     moduleName,
		nameToHostFunc: map[string]*wasm.HostFunc{},
	}
}

// hostFunctionBuilder implements HostFunctionBuilder
type hostFunctionBuilder struct {
	b           *hostModuleBuilder
	fn          interface{}
	name        string
	paramNames  []string
	resultNames []string
}

// WithGoFunction implements HostFunctionBuilder.WithGoFunction
func (h *hostFunctionBuilder) WithGoFunction(fn api.GoFunction, params, results []api.ValueType) HostFunctionBuilder {
	h.fn = &wasm.HostFunc{ParamTypes: wasm.FromApiValueType(params), ResultTypes: wasm.FromApiValueType(results), Code: wasm.Code{GoFunc: fn}}
	return h
}

// WithGoModuleFunction implements HostFunctionBuilder.WithGoModuleFunction
func (h *hostFunctionBuilder) WithGoModuleFunction(fn api.GoModuleFunction, params, results []api.ValueType) HostFunctionBuilder {
	h.fn = &wasm.HostFunc{ParamTypes: wasm.FromApiValueType(params), ResultTypes: wasm.FromApiValueType(results), Code: wasm.Code{GoFunc: fn}}
	return h
}

// WithName implements HostFunctionBuilder.WithName
func (h *hostFunctionBuilder) WithName(name string) HostFunctionBuilder {
	h.name = name
	return h
}

// WithParameterNames implements HostFunctionBuilder.WithParameterNames
func (h *hostFunctionBuilder) WithParameterNames(names ...string) HostFunctionBuilder {
	h.paramNames = names
	return h
}

// WithResultNames implements HostFunctionBuilder.WithResultNames
func (h *hostFunctionBuilder) WithResultNames(names ...string) HostFunctionBuilder {
	h.resultNames = names
	return h
}

// Export implements HostFunctionBuilder.Export
func (h *hostFunctionBuilder) Export(exportName string) HostModuleBuilder {
	var hostFn *wasm.HostFunc
	if fn, ok := h.fn.(*wasm.HostFunc); ok {
		hostFn = fn
	} else {
		hostFn = &wasm.HostFunc{Code: wasm.Code{GoFunc: h.fn}}
	}

	// Assign any names from the builder
	hostFn.ExportName = exportName
	if h.name != "" {
		hostFn.Name = h.name
	}
	if len(h.paramNames) != 0 {
		hostFn.ParamNames = h.paramNames
	}
	if len(h.resultNames) != 0 {
		hostFn.ResultNames = h.resultNames
	}

	h.b.ExportHostFunc(hostFn)
	return h.b
}

// ExportHostFunc implements wasm.HostFuncExporter
func (b *hostModuleBuilder) ExportHostFunc(fn *wasm.HostFunc) {
	if _, ok := b.nameToHostFunc[fn.ExportName]; !ok { // add a new name
		b.exportNames = append(b.exportNames, fn.ExportName)
	}
	b.nameToHostFunc[fn.ExportName] = fn
}

// NewFunctionBuilder implements HostModuleBuilder.NewFunctionBuilder
func (b *hostModuleBuilder) NewFunctionBuilder() HostFunctionBuilder {
	return &hostFunctionBuilder{b: b}
}

// Compile implements HostModuleBuilder.Compile
func (b *hostModuleBuilder) Compile(ctx context.Context) (CompiledModule, error) {
	module, err := wasm.NewHostModule(b.moduleName, b.exportNames, b.nameToHostFunc, b.r.enabledFeatures)
	if err != nil {
		return nil, err
	} else if err = module.Validate(b.r.enabledFeatures); err != nil {
		return nil, err
	}

	c := &compiledModule{module: module, compiledEngine: b.r.store.Engine}
	listeners, err := buildFunctionListeners(ctx, module)
	if err != nil {
		return nil, err
	}

	if err = b.r.store.Engine.CompileModule(ctx, module, listeners, false); err != nil {
		return nil, err
	}

	// typeIDs are static and compile-time known.
	typeIDs, err := b.r.store.GetFunctionTypeIDs(module.TypeSection)
	if err != nil {
		return nil, err
	}
	c.typeIDs = typeIDs

	return c, nil
}

// hostModuleInstance is a wrapper around api.Module that prevents calling ExportedFunction.
type hostModuleInstance struct{ api.Module }

// ExportedFunction implements api.Module ExportedFunction.
func (h hostModuleInstance) ExportedFunction(name string) api.Function {
	panic("calling ExportedFunction is forbidden on host modules. See the note on ExportedFunction interface")
}

// UnwrapModuleInstance returns the raw *wasm.ModuleInstance this host module
// wraps, for internal import-resolution wiring only: registering a host module
// in an api.ImportResolver hands ModuleInstance.resolveImports a value
// it type-asserts to *wasm.ModuleInstance, which this wrapper is not. The method
// signature names an internal type, so code outside this module cannot express
// the interface to call it -- the public ExportedFunction-forbidden guarantee is
// unaffected. (resolveImports links via internal module state, not
// ExportedFunction, so a resolver-provided host module is never called that way.)
func (h hostModuleInstance) UnwrapModuleInstance() *wasm.ModuleInstance {
	return h.Module.(*wasm.ModuleInstance)
}

// Instantiate implements HostModuleBuilder.Instantiate
func (b *hostModuleBuilder) Instantiate(ctx context.Context) (api.Module, error) {
	if compiled, err := b.Compile(ctx); err != nil {
		return nil, err
	} else {
		compiled.(*compiledModule).closeWithModule = true
		m, err := b.r.InstantiateModule(ctx, compiled, NewModuleConfig())
		if err != nil {
			return nil, err
		}
		return hostModuleInstance{m}, nil
	}
}
