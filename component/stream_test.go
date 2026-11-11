package component_test

import (
	"testing"

	"github.com/samyfodil/wazy/component"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// TypeTable.Stream and Future name their element, or none for the zero
// TypeRef: a bare stream or future.
func TestTypeTable_StreamFuture(t *testing.T) {
	tbl := component.NewTypeTable()
	resolve := tbl.Resolver()
	u8 := component.Prim("u8")
	for _, c := range []struct {
		ref  component.TypeRef
		want component.TypeDesc
	}{
		{tbl.Stream(u8), component.StreamDesc{Element: &u8}},
		{tbl.Stream(component.TypeRef{}), component.StreamDesc{}},
		{tbl.Future(u8), component.FutureDesc{Element: &u8}},
		{tbl.Future(component.TypeRef{}), component.FutureDesc{}},
	} {
		require.Equal(t, c.want, resolve(*c.ref.TypeIndex))
	}
}

func TestNewErrorContext(t *testing.T) {
	require.Equal(t, "boom", component.NewErrorContext("boom").DebugMessage())
}
