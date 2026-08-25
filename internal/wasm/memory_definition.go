package wasm

import (
	"math"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/internalapi"
)

// ImportedMemories implements the same method as documented on wazy.CompiledModule.
func (m *Module) ImportedMemories() (ret []api.MemoryDefinition) {
	for i := range m.MemoryDefinitionSection {
		d := &m.MemoryDefinitionSection[i]
		if d.importDesc != nil {
			ret = append(ret, d)
		}
	}
	return
}

// ExportedMemories implements the same method as documented on wazy.CompiledModule.
func (m *Module) ExportedMemories() map[string]api.MemoryDefinition {
	ret := map[string]api.MemoryDefinition{}
	for i := range m.MemoryDefinitionSection {
		d := &m.MemoryDefinitionSection[i]
		for _, e := range d.exportNames {
			ret[e] = d
		}
	}
	return ret
}

// BuildMemoryDefinitions generates memory metadata that can be parsed from
// the module. This must be called after all validation.
//
// Note: This is exported for wazy.Runtime `CompileModule`.
func (m *Module) BuildMemoryDefinitions() {
	var moduleName string
	if m.NameSection != nil {
		moduleName = m.NameSection.ModuleName
	}

	memoryCount := m.ImportMemoryCount + Index(len(m.MemorySection))

	if memoryCount == 0 {
		return
	}

	m.MemoryDefinitionSection = make([]MemoryDefinition, 0, memoryCount)
	importMemIdx := Index(0)
	for i := range m.ImportSection {
		imp := &m.ImportSection[i]
		if imp.Type != ExternTypeMemory {
			continue
		}

		m.MemoryDefinitionSection = append(m.MemoryDefinitionSection, MemoryDefinition{
			importDesc: &[2]string{imp.Module, imp.Name},
			index:      importMemIdx,
			memory:     imp.DescMem,
		})
		importMemIdx++
	}

	for i := range m.MemorySection {
		m.MemoryDefinitionSection = append(m.MemoryDefinitionSection, MemoryDefinition{
			index:  importMemIdx,
			memory: &m.MemorySection[i],
		})
		importMemIdx++
	}

	for i := range m.MemoryDefinitionSection {
		d := &m.MemoryDefinitionSection[i]
		d.moduleName = moduleName
		for i := range m.ExportSection {
			e := &m.ExportSection[i]
			if e.Type == ExternTypeMemory && e.Index == d.index {
				d.exportNames = append(d.exportNames, e.Name)
			}
		}
	}
}

// MemoryDefinition implements api.MemoryDefinition
type MemoryDefinition struct {
	internalapi.WazyOnlyType
	moduleName  string
	index       Index
	importDesc  *[2]string
	exportNames []string
	memory      *Memory
}

// ModuleName implements the same method as documented on api.MemoryDefinition.
func (f *MemoryDefinition) ModuleName() string {
	return f.moduleName
}

// Index implements the same method as documented on api.MemoryDefinition.
func (f *MemoryDefinition) Index() uint32 {
	return f.index
}

// Import implements the same method as documented on api.MemoryDefinition.
func (f *MemoryDefinition) Import() (moduleName, name string, isImport bool) {
	if importDesc := f.importDesc; importDesc != nil {
		moduleName, name, isImport = importDesc[0], importDesc[1], true
	}
	return
}

// ExportNames implements the same method as documented on api.MemoryDefinition.
func (f *MemoryDefinition) ExportNames() []string {
	return f.exportNames
}

// Min implements the same method as documented on api.MemoryDefinition.
func (f *MemoryDefinition) Min() uint32 {
	return saturateToUint32(f.memory.Min)
}

// Max implements the same method as documented on api.MemoryDefinition.
func (f *MemoryDefinition) Max() (max uint32, encoded bool) {
	max = saturateToUint32(f.memory.Max)
	encoded = f.memory.IsMaxEncoded
	return
}

// saturateToUint32 caps rather than truncates a 64-bit memory's page count for
// the uint32 halves of api.MemoryDefinition. Truncation would be worse than
// imprecise: a memory declared with a maximum of exactly 2^32 pages would
// report a maximum of zero, which reads as "cannot grow" -- the opposite of the
// truth, and the very thing a caller consults Max to decide. Saturating keeps
// the bound in the direction it is meant to be read, and keeps Min <= Max.
// Use Min64 and Max64 for the exact values.
func saturateToUint32(pages uint64) uint32 {
	return uint32(min(pages, math.MaxUint32))
}

// Min64 implements the same method as documented on api.MemoryDefinition.
func (f *MemoryDefinition) Min64() uint64 {
	return f.memory.Min
}

// Max64 implements the same method as documented on api.MemoryDefinition.
func (f *MemoryDefinition) Max64() (max uint64, encoded bool) {
	max = f.memory.Max
	encoded = f.memory.IsMaxEncoded
	return
}

// IsMemory64 implements the same method as documented on api.MemoryDefinition.
func (f *MemoryDefinition) IsMemory64() bool {
	return f.memory.IsMemory64
}
