package wasm

import (
	"context"
	"fmt"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/sys"
)

// FailIfClosed returns a sys.ExitError if CloseWithExitCode was called.
func (m *ModuleInstance) FailIfClosed() (err error) {
	if closed := m.Closed.Load(); closed != 0 {
		return sys.NewExitError(uint32(closed >> 32)) // Unpack the high order bits as the exit code.
	}
	return nil
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

// closeWithExitCode is the same as CloseWithExitCode besides this doesn't delete it from Store.moduleList.
func (m *ModuleInstance) closeWithExitCode(ctx context.Context, exitCode uint32) (err error) {
	if !m.setExitCode(exitCode, exitCodeFlagResourceClosed) {
		return nil // not an error to have already closed
	}
	return m.ensureResourcesClosed(ctx)
}

type exitCodeFlag = uint64

// exitCodeFlagResourceClosed indicates that the module was closed and resources were already closed.
const exitCodeFlagResourceClosed = 1

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

	for _, mem := range m.Memories {
		if mem.ownerModuleEngine == m.Engine {
			// Owner close. Mark ownerClosed, and recycle Buffer to the pool now
			// only if no importer is still live -- otherwise the LAST importer's
			// Close recycles it (the importer branch below). The claim (take the
			// slice, set released) happens under Mux so exactly one close pools it.
			// expBuffer (custom allocator) is owner-only and freed unconditionally,
			// exactly as before; poolable is false for it and for shared memories.
			poolAuditRelease(mem, m)
			mem.Mux.Lock()
			mem.ownerClosed = true
			var recycle []byte
			// released doubles as the claim that made Buffer != nil work
			// before: set under Mux, so exactly one close -- this one or the
			// last importer's -- releases the storage however many run
			// concurrently. Setting it here rather than just before the Put
			// also means a reader racing this close sees an empty memory from
			// the moment the buffer is spoken for, not from the moment it
			// actually lands in the pool.
			var free api.LinearMemory
			var deferred bool
			if mem.expBuffer != nil || (mem.poolable && mem.importers == 0) {
				// Take the slice BEFORE the claim: byteSize reports 0 once
				// released is set, so claiming first would pool an empty one.
				candidate := mem.allocatedBuffer()[:mem.byteSize()]
				if !mem.released.Swap(true) {
					if mem.expBuffer != nil {
						free, mem.expBuffer = mem.expBuffer, nil
					} else {
						recycle = candidate
						poolAuditPut(mem)
					}
					// A call still executing reads and writes this buffer
					// directly, without ever consulting released -- so hand the
					// release to whichever call leaves last rather than pulling
					// the memory out from under it. See callers.
					if mem.anyCallInFlightLocked() {
						// Stash the WHOLE backing, not candidate: that call can
						// still grow the memory and write above candidate's
						// length, and the pool clears only what it is handed.
						// exitAfterRelease takes the prefix once it has drained.
						mem.pendingRelease.recycle, mem.pendingRelease.free = mem.allocatedBuffer(), free
						if free != nil {
							mem.pendingRelease.recycle = nil
						}
						recycle, free = nil, nil
						deferred = true
					}
				}
			}
			mem.Mux.Unlock()

			if deferred {
				// The last call may have left between the check and the store,
				// in which case nobody is coming back for it.
				mem.exitAfterRelease()
			} else if free != nil {
				free.Free()
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
			poolAuditRelease(mem, m)
			mem.Mux.Lock()
			if mem.importers > 0 {
				mem.importers--
			}
			var recycle []byte
			var deferred bool
			if mem.ownerClosed && mem.importers == 0 && mem.poolable {
				// Slice first, claim second -- see the owner branch above.
				candidate := mem.allocatedBuffer()[:mem.byteSize()]
				if !mem.released.Swap(true) {
					recycle = candidate
					poolAuditPut(mem)
					if mem.anyCallInFlightLocked() {
						// Whole backing; see the owner branch above.
						mem.pendingRelease.recycle = mem.allocatedBuffer()
						recycle, deferred = nil, true
					}
				}
			}
			mem.Mux.Unlock()
			if deferred {
				mem.exitAfterRelease()
			} else if recycle != nil {
				putPooledMemoryBuffer(recycle)
			}
		}
	}

	// Memories this module could reach through a function or table import but
	// does not own. Dropping the hold can make this the last holder, in which
	// case the owner's deferred recycle happens here. See holdMemoriesOf.
	for _, mem := range m.heldMemories {
		mem.Mux.Lock()
		if mem.importers > 0 {
			mem.importers--
		}
		last := mem.ownerClosed && mem.importers == 0 && mem.poolable
		mem.Mux.Unlock()
		poolAuditRelease(mem, m)
		if last {
			mem.releaseAfterLastHolder()
		}
	}
	m.heldMemories = nil

	if m.CodeCloser != nil {
		if e := m.CodeCloser.Close(ctx); err == nil {
			err = e
		}
		m.CodeCloser = nil
	}
	return err
}

// Memory implements the same method as documented on api.Module. It returns
// memory index 0, the module's first declared or imported memory.
func (m *ModuleInstance) Memory() api.Memory {
	if len(m.Memories) == 0 {
		return nil
	}
	return m.Memories[0]
}

// ExportedMemory implements the same method as documented on api.Module.
func (m *ModuleInstance) ExportedMemory(name string) api.Memory {
	exp, err := m.getExport(name, ExternTypeMemory)
	if err != nil {
		return nil
	}
	return m.Memories[exp.Index]
}

// ExportedMemoryDefinitions implements the same method as documented on
// api.Module.
func (m *ModuleInstance) ExportedMemoryDefinitions() map[string]api.MemoryDefinition {
	ret := map[string]api.MemoryDefinition{}
	for name, exp := range m.Exports {
		if exp.Type == ExternTypeMemory {
			ret[name] = m.Memories[exp.Index].definition
		}
	}
	return ret
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
