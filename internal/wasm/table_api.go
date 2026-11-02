package wasm

import (
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/internalapi"
)

// ExportedTable implements the same method as documented on api.Module.
func (m *ModuleInstance) ExportedTable(name string) api.Table {
	exp, err := m.getExport(name, ExternTypeTable)
	if err != nil {
		return nil
	}
	// The type as this module declares it: a concrete reference names a type in this module's type section,
	// whichever module defined the table.
	elem, types := m.tableType(exp.Index), m.Source.TypeSection
	return &exportedTable{
		m:         m,
		t:         m.Tables[exp.Index],
		externs:   isRefSubtypeOf(elem, RefTypeExternref, types),
		anyExtern: isRefSubtypeOf(RefTypeExternref, elem.AsNullable(), types),
		funcs:     isRefSubtypeOf(elem, RefTypeFuncref, types),
		nullable:  elem.IsNullable(),
	}
}

// tableType returns the type of table idx as m declares it.
func (m *ModuleInstance) tableType(idx Index) ValueType {
	src := m.Source
	if idx >= src.ImportTableCount {
		return src.TableSection[idx-src.ImportTableCount].Type
	}
	for i := range src.ImportSection {
		if imp := &src.ImportSection[i]; imp.Type == ExternTypeTable && imp.IndexPerType == idx {
			return imp.DescTable.Type
		}
	}
	panic("BUG: imported table without an import")
}

// exportedTable implements api.Table.
type exportedTable struct {
	internalapi.WazyOnlyType

	m *ModuleInstance
	t *TableInstance

	// externs is whether every entry is an externref, which Get may return. anyExtern is whether the table can
	// hold any externref, which Set and Grow may store: not so for (ref null noextern), whose entries are all null.
	externs, anyExtern bool
	// funcs is whether every entry is a function reference, which Function may look up.
	funcs bool
	// nullable is whether an entry may be null.
	nullable bool
}

// Size implements api.Table.Size.
func (e *exportedTable) Size() uint32 {
	return uint32(len(e.t.References))
}

// Grow implements api.Table.Grow.
func (e *exportedTable) Grow(delta uint32, init uint64) (uint32, bool) {
	if delta > 0 && (init == 0 && !e.nullable || init != 0 && !e.anyExtern) {
		return 0, false
	}
	if prev := e.t.Grow(delta, Reference(init)); prev != 0xffffffff {
		return prev, true
	}
	return 0, false
}

// Get implements api.Table.Get.
func (e *exportedTable) Get(index uint32) (uint64, bool) {
	if !e.externs || uint64(index) >= uint64(len(e.t.References)) {
		return 0, false
	}
	return uint64(e.t.References[index]), true
}

// Set implements api.Table.Set.
func (e *exportedTable) Set(index uint32, v uint64) bool {
	if !e.anyExtern || v == 0 && !e.nullable || uint64(index) >= uint64(len(e.t.References)) {
		return false
	}
	e.t.References[index] = Reference(v)
	return true
}

// Function implements api.Table.Function.
func (e *exportedTable) Function(index uint32) api.Function {
	if !e.funcs || uint64(index) >= uint64(len(e.t.References)) {
		return nil
	}
	ref := e.t.References[index]
	if ref == 0 {
		return nil
	}
	// LookupFunction checks the entry against an expected type, as call_indirect does; the entry's own type
	// always passes.
	return e.m.LookupFunction(e.t, e.m.Engine.TypeIDOfReference(ref), index)
}
