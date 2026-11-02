// Package emscripten contains Go-defined special functions imported by
// Emscripten under the module name "env".
//
// Emscripten has many imports which are triggered on build flags. Use
// FunctionExporter, instead of Instantiate, to define more "env" functions.
//
// # Relationship to WASI
//
// Emscripten typically requires wasi_snapshot_preview1 to implement exit.
//
// See wasi_snapshot_preview1.Instantiate and
// https://github.com/emscripten-core/emscripten/wiki/WebAssembly-Standalone
package emscripten

import (
	"context"
	"strings"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	internal "github.com/samyfodil/wazy/internal/emscripten"
	"github.com/samyfodil/wazy/internal/wasm"
)

const i32 = wasm.ValueTypeI32

// MustInstantiate calls Instantiate or panics on error.
//
// This is a simpler function for those who know the module "env" is not
// already instantiated, and don't need to unload it.
//
// Deprecated: Due to Emscripten dynamic import generation, InstantiateForModule should be used instead.
func MustInstantiate(ctx context.Context, r wazy.Runtime) {
	if _, err := Instantiate(ctx, r); err != nil {
		panic(err)
	}
}

// Instantiate instantiates the "env" module used by Emscripten into the
// runtime.
//
// # Notes
//
//   - Failure cases are documented on wazy.Runtime InstantiateModule.
//   - Closing the wazy.Runtime has the same effect as closing the result.
//   - To add more functions to the "env" module, use FunctionExporter.
//
// Deprecated: Due to Emscripten dynamic import generation, InstantiateForModule should be used instead.
func Instantiate(ctx context.Context, r wazy.Runtime) (api.Closer, error) {
	builder := r.NewHostModuleBuilder("env")
	NewFunctionExporter().ExportFunctions(builder)
	return builder.Instantiate(ctx)
}

// FunctionExporter configures the functions in the "env" module used by
// Emscripten.
//
// # Notes
//
//   - This is an interface for decoupling, not third-party implementations.
//     All implementations are in wazy.
type FunctionExporter interface {
	// ExportFunctions builds functions to export with a wazy.HostModuleBuilder
	// named "env".
	ExportFunctions(wazy.HostModuleBuilder)
}

// NewFunctionExporter returns a FunctionExporter object with trace disabled.
// Deprecated: Due to Emscripten dynamic import generation, NewFunctionExporterForModule should be used instead.
func NewFunctionExporter() FunctionExporter {
	return &functionExporter{}
}

type functionExporter struct{}

// ExportFunctions implements FunctionExporter.ExportFunctions
func (functionExporter) ExportFunctions(builder wazy.HostModuleBuilder) {
	exporter := builder.(wasm.HostFuncExporter)
	exporter.ExportHostFunc(internal.NotifyMemoryGrowth)
}

type emscriptenFns []*wasm.HostFunc

// emscriptenExports is what a guest-specific FunctionExporter adds to "env".
type emscriptenExports struct {
	fns  emscriptenFns
	tags []wasm.HostTag
}

// InstantiateForModule instantiates a module named "env" populated with any
// known functions used in emscripten.
func InstantiateForModule(ctx context.Context, r wazy.Runtime, guest wazy.CompiledModule) (api.Closer, error) {
	// Create the exporter for the supplied wasm
	exporter, err := NewFunctionExporterForModule(guest)
	if err != nil {
		return nil, err
	}

	// Instantiate it!
	env := r.NewHostModuleBuilder("env")
	exporter.ExportFunctions(env)
	return env.Instantiate(ctx)
}

// NewFunctionExporterForModule returns a guest-specific FunctionExporter,
// populated with any known functions used in emscripten, and the
// exception-handling tags a dynamic-linking build imports from "env".
func NewFunctionExporterForModule(guest wazy.CompiledModule) (FunctionExporter, error) {
	ret := emscriptenFns{}
	for _, fn := range guest.ImportedFunctions() {
		importModule, importName, isImport := fn.Import()
		if !isImport || importModule != "env" {
			continue // not emscripten
		}
		if importName == internal.FunctionNotifyMemoryGrowth {
			ret = append(ret, internal.NotifyMemoryGrowth)
			continue
		}
		if importName == internal.FunctionThrowLongjmp {
			ret = append(ret, internal.ThrowLongjmp)
			continue
		}
		if !strings.HasPrefix(importName, internal.InvokePrefix) {
			continue // not invoke, and maybe not emscripten
		}

		hf := internal.NewInvokeFunc(importName, fn.ParamTypes(), fn.ResultTypes())
		ret = append(ret, hf)
	}
	return &emscriptenExports{fns: ret, tags: importedTags(guest)}, nil
}

// importedTags returns the tags emscripten defines that guest imports from "env", each with the type guest
// declares for it: the parameter is a pointer, an i64 in a wasm64 build.
func importedTags(guest wazy.CompiledModule) (tags []wasm.HostTag) {
	m, ok := guest.(interface{ WasmModule() *wasm.Module })
	if !ok {
		return nil
	}
	module := m.WasmModule()
	for i := range module.ImportSection {
		imp := &module.ImportSection[i]
		if imp.Type != wasm.ExternTypeTag || imp.Module != "env" ||
			(imp.Name != internal.TagCppException && imp.Name != internal.TagCLongjmp) {
			continue
		}
		tags = append(tags, wasm.HostTag{ExportName: imp.Name, ParamTypes: module.TypeSection[imp.DescTag].Params})
	}
	return tags
}

// ExportFunctions implements FunctionExporter.ExportFunctions
func (i emscriptenFns) ExportFunctions(builder wazy.HostModuleBuilder) {
	exporter := builder.(wasm.HostFuncExporter)
	for _, fn := range i {
		exporter.ExportHostFunc(fn)
	}
}

// ExportFunctions implements FunctionExporter.ExportFunctions. It exports the guest's tags too.
func (e *emscriptenExports) ExportFunctions(builder wazy.HostModuleBuilder) {
	e.fns.ExportFunctions(builder)
	exporter := builder.(wasm.HostTagExporter)
	for _, tag := range e.tags {
		exporter.ExportHostTag(tag)
	}
}
