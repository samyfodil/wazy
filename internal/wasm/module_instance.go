package wasm

import (
	"context"
	"errors"
	"fmt"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/sys"
)

// FailIfClosed returns a sys.ExitError if CloseWithExitCode was called.
func (m *ModuleInstance) FailIfClosed() (err error) {
	if closed := m.Closed.Load(); closed != 0 {
		switch closed & exitCodeFlagMask {
		case exitCodeFlagResourceClosed:
		case exitCodeFlagResourceNotClosed:
			// This happens when this module is closed asynchronously in CloseModuleOnCanceledOrTimeout,
			// and the closure of resources have been deferred here.
			_ = m.ensureResourcesClosed(context.Background())
		}
		return sys.NewExitError(uint32(closed >> 32)) // Unpack the high order bits as the exit code.
	}
	return nil
}

// CloseModuleOnCanceledOrTimeout take a context `ctx`, which might be a Cancel or Timeout context,
// and spawns the Goroutine to check the context is canceled ot deadline exceeded. If it reaches
// one of the conditions, it sets the appropriate exit code.
//
// Callers of this function must invoke the returned context.CancelFunc to release the spawned Goroutine.
func (m *ModuleInstance) CloseModuleOnCanceledOrTimeout(ctx context.Context) context.CancelFunc {
	// Creating an empty channel in this case is a bit more efficient than
	// creating a context.Context and canceling it with the same effect. We
	// really just need to be notified when to stop listening to the users
	// context. Closing the channel will unblock the select in the goroutine
	// causing it to return an stop listening to ctx.Done().
	cancelChan := make(chan struct{})
	go m.closeModuleOnCanceledOrTimeout(ctx, cancelChan)
	return func() { close(cancelChan) }
}

// closeModuleOnCanceledOrTimeout is extracted from CloseModuleOnCanceledOrTimeout for testing.
func (m *ModuleInstance) closeModuleOnCanceledOrTimeout(ctx context.Context, cancelChan <-chan struct{}) {
	select {
	case <-ctx.Done():
		select {
		case <-cancelChan:
			// In some cases by the time this goroutine is scheduled, the caller
			// has already closed both the context and the cancelChan. In this
			// case go will randomize which branch of the outer select to enter
			// and we don't want to close the module.
		default:
			// This is the same logic as CloseWithCtxErr except this calls closeWithExitCodeWithoutClosingResource
			// so that we can defer the resource closure in FailIfClosed.
			switch {
			case errors.Is(ctx.Err(), context.Canceled):
				// TODO: figure out how to report error here.
				_ = m.closeWithExitCodeWithoutClosingResource(sys.ExitCodeContextCanceled)
			case errors.Is(ctx.Err(), context.DeadlineExceeded):
				// TODO: figure out how to report error here.
				_ = m.closeWithExitCodeWithoutClosingResource(sys.ExitCodeDeadlineExceeded)
			}
		}
	case <-cancelChan:
	}
}

// CloseWithCtxErr closes the module with an exit code based on the type of
// error reported by the context.
//
// If the context's error is unknown or nil, the module does not close.
func (m *ModuleInstance) CloseWithCtxErr(ctx context.Context) {
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		// TODO: figure out how to report error here.
		_ = m.CloseWithExitCode(ctx, sys.ExitCodeContextCanceled)
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		// TODO: figure out how to report error here.
		_ = m.CloseWithExitCode(ctx, sys.ExitCodeDeadlineExceeded)
	}
}

// Name implements the same method as documented on api.Module
func (m *ModuleInstance) Name() string {
	return m.ModuleName
}

// String implements the same method as documented on api.Module
func (m *ModuleInstance) String() string {
	return fmt.Sprintf("Module[%s]", m.Name())
}

// Close implements the same method as documented on api.Module.
func (m *ModuleInstance) Close(ctx context.Context) (err error) {
	return m.CloseWithExitCode(ctx, 0)
}

// CloseWithExitCode implements the same method as documented on api.Module.
func (m *ModuleInstance) CloseWithExitCode(ctx context.Context, exitCode uint32) (err error) {
	if !m.setExitCode(exitCode, exitCodeFlagResourceClosed) {
		return nil // not an error to have already closed
	}
	_ = m.s.deleteModule(m)
	return m.ensureResourcesClosed(ctx)
}

// IsClosed implements the same method as documented on api.Module.
func (m *ModuleInstance) IsClosed() bool {
	return m.Closed.Load() != 0
}

func (m *ModuleInstance) closeWithExitCodeWithoutClosingResource(exitCode uint32) (err error) {
	if !m.setExitCode(exitCode, exitCodeFlagResourceNotClosed) {
		return nil // not an error to have already closed
	}
	_ = m.s.deleteModule(m)
	return nil
}

// closeWithExitCode is the same as CloseWithExitCode besides this doesn't delete it from Store.moduleList.
func (m *ModuleInstance) closeWithExitCode(ctx context.Context, exitCode uint32) (err error) {
	if !m.setExitCode(exitCode, exitCodeFlagResourceClosed) {
		return nil // not an error to have already closed
	}
	return m.ensureResourcesClosed(ctx)
}

type exitCodeFlag = uint64

const exitCodeFlagMask = 0xff

const (
	// exitCodeFlagResourceClosed indicates that the module was closed and resources were already closed.
	exitCodeFlagResourceClosed = 1 << iota
	// exitCodeFlagResourceNotClosed indicates that the module was closed while resources are not closed yet.
	exitCodeFlagResourceNotClosed
)

func (m *ModuleInstance) setExitCode(exitCode uint32, flag exitCodeFlag) bool {
	closed := flag | uint64(exitCode)<<32 // Store exitCode as high-order bits.
	return m.Closed.CompareAndSwap(0, closed)
}

// ensureResourcesClosed ensures that resources assigned to ModuleInstance is released.
// Only one call will happen per module, due to external atomic guards on Closed.
func (m *ModuleInstance) ensureResourcesClosed(ctx context.Context) (err error) {
	if closeNotifier := m.CloseNotifier; closeNotifier != nil { // experimental
		closeNotifier.CloseNotify(ctx, uint32(m.Closed.Load()>>32))
		m.CloseNotifier = nil
	}

	if sysCtx := m.Sys; sysCtx != nil { // nil if from HostModuleBuilder
		err = sysCtx.FS().Close()
		m.Sys = nil
	}

	if mem := m.MemoryInstance; mem != nil {
		if mem.ownerModuleEngine == m.Engine {
			// Owner close. Mark ownerClosed, and recycle Buffer to the pool now
			// only if no importer is still live -- otherwise the LAST importer's
			// Close recycles it (the importer branch below). The claim (take
			// Buffer, set it nil) happens under Mux so exactly one close pools it.
			// expBuffer (custom allocator) is owner-only and freed unconditionally,
			// exactly as before; poolable is false for it and for shared memories.
			mem.Mux.Lock()
			mem.ownerClosed = true
			var recycle []byte
			if mem.poolable && mem.importers == 0 && mem.Buffer != nil {
				recycle = mem.Buffer
				// Drop our own reference so a stale post-Close read of this (now
				// closed) MemoryInstance -- e.g. through an api.Memory the caller
				// kept past Close, already a misuse -- sees an empty memory rather
				// than whatever unrelated module the pool later hands this array to.
				mem.Buffer = nil
			}
			mem.Mux.Unlock()

			if mem.expBuffer != nil {
				mem.expBuffer.Free()
				mem.expBuffer = nil
			} else if recycle != nil {
				putPooledMemoryBuffer(recycle)
			}
		} else {
			// Importer close (mem != nil and we are not the owner => this module
			// imported the memory and incremented importers in resolveImports).
			// Drop our reference; if we were the LAST importer and the owner has
			// already closed, it deferred recycling to us -- pool Buffer now
			// (claim under Mux, single-shot via the Buffer != nil guard). We never
			// touch expBuffer/shared memories (poolable == false).
			mem.Mux.Lock()
			if mem.importers > 0 {
				mem.importers--
			}
			var recycle []byte
			if mem.ownerClosed && mem.importers == 0 && mem.poolable && mem.Buffer != nil {
				recycle = mem.Buffer
				mem.Buffer = nil
			}
			mem.Mux.Unlock()
			if recycle != nil {
				putPooledMemoryBuffer(recycle)
			}
		}
	}

	if m.CodeCloser != nil {
		if e := m.CodeCloser.Close(ctx); err == nil {
			err = e
		}
		m.CodeCloser = nil
	}
	return err
}

// Memory implements the same method as documented on api.Module.
func (m *ModuleInstance) Memory() api.Memory {
	return m.MemoryInstance
}

// ExportedMemory implements the same method as documented on api.Module.
func (m *ModuleInstance) ExportedMemory(name string) api.Memory {
	_, err := m.getExport(name, ExternTypeMemory)
	if err != nil {
		return nil
	}
	// We Assume that we have at most one memory.
	return m.MemoryInstance
}

// ExportedMemoryDefinitions implements the same method as documented on
// api.Module.
func (m *ModuleInstance) ExportedMemoryDefinitions() map[string]api.MemoryDefinition {
	// Special case as we currently only support one memory.
	if mem := m.MemoryInstance; mem != nil {
		// Now, find out if it is exported
		for name, exp := range m.Exports {
			if exp.Type == ExternTypeMemory {
				return map[string]api.MemoryDefinition{name: mem.definition}
			}
		}
	}
	return map[string]api.MemoryDefinition{}
}

// ExportedFunction implements the same method as documented on api.Module.
func (m *ModuleInstance) ExportedFunction(name string) api.Function {
	exp, err := m.getExport(name, ExternTypeFunc)
	if err != nil {
		return nil
	}
	return m.Engine.NewFunction(exp.Index)
}

// ExportedFunctionDefinitions implements the same method as documented on
// api.Module.
func (m *ModuleInstance) ExportedFunctionDefinitions() map[string]api.FunctionDefinition {
	result := map[string]api.FunctionDefinition{}
	for name, exp := range m.Exports {
		if exp.Type == ExternTypeFunc {
			result[name] = m.Source.FunctionDefinition(exp.Index)
		}
	}
	return result
}

// GlobalVal is an internal hack to get the lower 64 bits of a global.
func (m *ModuleInstance) GlobalVal(idx Index) uint64 {
	return m.Globals[idx].Val
}

// ExportedGlobal implements the same method as documented on api.Module.
func (m *ModuleInstance) ExportedGlobal(name string) api.Global {
	exp, err := m.getExport(name, ExternTypeGlobal)
	if err != nil {
		return nil
	}
	g := m.Globals[exp.Index]
	if g.Type.Mutable {
		return mutableGlobal{g: g}
	}
	return constantGlobal{g: g}
}

// NumGlobal implements api.InternalModule.
func (m *ModuleInstance) NumGlobal() int {
	return len(m.Globals)
}

// Global implements api.InternalModule.
func (m *ModuleInstance) Global(idx int) api.Global {
	return constantGlobal{g: m.Globals[idx]}
}
