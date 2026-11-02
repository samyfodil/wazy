package wasm

import (
	"testing"

	"github.com/samyfodil/wazy/internal/testing/require"
)

func TestModuleInstance_ExportedTable(t *testing.T) {
	types := []FunctionType{
		{Params: []ValueType{ValueTypeI32}},
		{CompositeKind: CompositeKindStruct, Fields: []FieldType{{Type: ValueTypeI32}}},
	}
	tests := []struct {
		name                      string
		elem                      ValueType
		externs, anyExtern, funcs bool
		nullable                  bool
	}{
		{name: "externref", elem: RefTypeExternref, externs: true, anyExtern: true, nullable: true},
		{name: "(ref extern)", elem: RefTypeExternref.AsNonNullable(), externs: true, anyExtern: true},
		{name: "(ref null noextern)", elem: ValueTypeNullExternref, externs: true, nullable: true},
		{name: "funcref", elem: RefTypeFuncref, funcs: true, nullable: true},
		{name: "(ref null nofunc)", elem: ValueTypeNullFuncref, funcs: true, nullable: true},
		{name: "(ref null $func)", elem: ValueTypeConcreteRef(0, true), funcs: true, nullable: true},
		{name: "(ref null $struct)", elem: ValueTypeConcreteRef(1, true), nullable: true},
		{name: "anyref", elem: ValueTypeAnyref, nullable: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := &ModuleInstance{
				Source:  &Module{TypeSection: types, TableSection: []Table{{Type: tc.elem}}},
				Tables:  []*TableInstance{{References: make([]Reference, 1), Type: tc.elem}},
				Exports: map[string]*Export{"t": {Type: ExternTypeTable, Name: "t", Index: 0}},
			}
			table := m.ExportedTable("t").(*exportedTable)
			require.Equal(t, tc.externs, table.externs)
			require.Equal(t, tc.anyExtern, table.anyExtern)
			require.Equal(t, tc.funcs, table.funcs)
			require.Equal(t, tc.nullable, table.nullable)

			_, ok := table.Get(0)
			require.Equal(t, tc.externs, ok)
			require.Equal(t, tc.anyExtern, table.Set(0, 5))
			require.Equal(t, tc.anyExtern && tc.nullable, table.Set(0, 0))
			_, ok = table.Grow(1, 5)
			require.Equal(t, tc.anyExtern, ok)
			_, ok = table.Grow(1, 0)
			require.Equal(t, tc.nullable, ok)
			// Growing by nothing never stores init, whatever it is.
			size, ok := table.Grow(0, 5)
			require.True(t, ok)
			require.Equal(t, table.Size(), size)
			// Every entry is null or an externref, so there is no function to find.
			require.Nil(t, table.Function(0))
		})
	}
}

func TestModuleInstance_tableType(t *testing.T) {
	m := &ModuleInstance{Source: &Module{
		ImportSection: []Import{
			{Type: ExternTypeFunc},
			{Type: ExternTypeTable, IndexPerType: 0, DescTable: Table{Type: RefTypeExternref}},
		},
		ImportTableCount: 1,
		TableSection:     []Table{{Type: RefTypeFuncref}},
	}}
	require.Equal(t, RefTypeExternref, m.tableType(0))
	require.Equal(t, RefTypeFuncref, m.tableType(1))
}
