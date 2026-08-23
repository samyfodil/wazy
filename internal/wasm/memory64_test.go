package wasm

import (
	"math"
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/testing/require"
)

func TestMemoryPagesToBytesNum_Saturates(t *testing.T) {
	// 2^48 pages is exactly 2^64 bytes, one past what a uint64 holds, and it is
	// the largest count a 64-bit memory may declare. Saturating keeps every
	// caller's "more than can ever be allocated" reading true.
	require.Equal(t, uint64(math.MaxUint64), MemoryPagesToBytesNum(Memory64LimitPages))
	require.Equal(t, uint64(math.MaxUint64), MemoryPagesToBytesNum(math.MaxUint64))
	require.Equal(t, uint64(1)<<63, MemoryPagesToBytesNum(1<<47))
}

func TestMemory_IndexType(t *testing.T) {
	require.Equal(t, ValueTypeI32, (&Memory{}).IndexType())
	require.Equal(t, ValueTypeI64, (&Memory{IsMemory64: true}).IndexType())
	require.Equal(t, ValueTypeI32, (&Table{}).IndexType())
	require.Equal(t, ValueTypeI64, (&Table{IsTable64: true}).IndexType())
	require.Equal(t, ValueTypeI32, (&MemoryInstance{}).indexType())
	require.Equal(t, ValueTypeI64, (&MemoryInstance{IsMemory64: true}).indexType())
	require.Equal(t, ValueTypeI32, (&TableInstance{}).indexType())
	require.Equal(t, ValueTypeI64, (&TableInstance{IsTable64: true}).indexType())
}

func TestMemory_Validate_Memory64Ceiling(t *testing.T) {
	// A 64-bit memory validates against the specification's own 2^48-page
	// ceiling, far above what any host allocates.
	m := &Memory{Min: Memory64LimitPages, Cap: Memory64LimitPages, Max: Memory64LimitPages, IsMemory64: true}
	require.NoError(t, m.Validate(Memory64LimitPages))

	over := &Memory{Min: Memory64LimitPages + 1, Cap: Memory64LimitPages + 1, Max: Memory64LimitPages + 1, IsMemory64: true}
	require.EqualError(t, over.Validate(Memory64LimitPages),
		"max 281474976710657 pages (16777216 Ti) over limit of 281474976710656 pages (16777216 Ti)")
}

func TestNewMemoryInstance_LimitPages(t *testing.T) {
	me := &mockModuleEngine{}
	t.Run("the embedder limit caps the declared maximum", func(t *testing.T) {
		// A 64-bit memory may declare far more than the host allows; the
		// instance's Max is the smaller of the two, so growth stops there.
		mem := &Memory{Min: 0, Cap: 0, Max: Memory64LimitPages, IsMemory64: true}
		m := NewMemoryInstance(mem, nil, me, 3)
		require.Equal(t, uint64(3), m.Max)
		require.True(t, m.IsMemory64)

		_, ok := m.Grow64(3)
		require.True(t, ok)
		_, ok = m.Grow64(1)
		require.False(t, ok)
	})
	t.Run("the module maximum still wins when it is smaller", func(t *testing.T) {
		mem := &Memory{Min: 0, Cap: 0, Max: 2, IsMemory64: true}
		m := NewMemoryInstance(mem, nil, me, Memory64LimitPages)
		require.Equal(t, uint64(2), m.Max)
	})
	t.Run("capacity is clamped but never below the minimum", func(t *testing.T) {
		mem := &Memory{Min: 1, Cap: 10, Max: 10}
		m := NewMemoryInstance(mem, nil, me, 2)
		require.Equal(t, uint64(2), m.Max)
		require.Equal(t, uint64(2), m.Cap)
	})
}

func TestMemoryInstance_Grow64(t *testing.T) {
	me := &mockModuleEngine{}
	newMem := func(min, max uint64) *MemoryInstance {
		return NewMemoryInstance(&Memory{Min: min, Cap: min, Max: max, IsMemory64: true}, nil, me, max)
	}

	t.Run("a delta that would wrap the page count fails", func(t *testing.T) {
		m := newMem(1, 4)
		// 1 + (2^64-1) wraps to 0, which would look in bounds if the addition
		// came first.
		_, ok := m.Grow64(math.MaxUint64)
		require.False(t, ok)
		require.Equal(t, uint64(1), m.Pages64())
	})
	t.Run("growing to exactly the maximum succeeds", func(t *testing.T) {
		m := newMem(1, 4)
		previous, ok := m.Grow64(3)
		require.True(t, ok)
		require.Equal(t, uint64(1), previous)
		require.Equal(t, uint64(4), m.Pages64())
		require.Equal(t, MemoryPagesToBytesNum(4), m.Size64())
	})
	t.Run("a zero delta reports the current size", func(t *testing.T) {
		m := newMem(2, 4)
		previous, ok := m.Grow64(0)
		require.True(t, ok)
		require.Equal(t, uint64(2), previous)
	})
	t.Run("a page count already past the maximum cannot underflow", func(t *testing.T) {
		// Only reachable for a hand-built instance: NewMemoryInstance never
		// produces one whose length exceeds Max.
		m := &MemoryInstance{Buffer: make([]byte, 2*MemoryPageSize), Max: 1, ownerModuleEngine: me}
		_, ok := m.Grow64(1)
		require.False(t, ok)
	})
	t.Run("Grow truncates the previous page count like Size does", func(t *testing.T) {
		m := &MemoryInstance{
			Max: 1 << 33, sizeBytes: MemoryPagesToBytesNum(1 << 32), ownerModuleEngine: me,
			Buffer: []byte{0}, IsMemory64: true,
		}
		previous, ok := m.Grow(0)
		require.True(t, ok)
		require.Equal(t, uint32(0), previous) // 2^32 truncated
		previous64, ok := m.Grow64(0)
		require.True(t, ok)
		require.Equal(t, uint64(1)<<32, previous64)
	})
}

func TestMemoryInstance_Read64Write64(t *testing.T) {
	me := &mockModuleEngine{}
	m := NewMemoryInstance(&Memory{Min: 1, Cap: 1, Max: 1, IsMemory64: true}, nil, me, 1)

	require.True(t, m.WriteString64(0, "wazy"))
	buf, ok := m.Read64(0, 4)
	require.True(t, ok)
	require.Equal(t, "wazy", string(buf))

	require.True(t, m.Write64(4, []byte{1, 2, 3}))
	buf, ok = m.Read64(4, 3)
	require.True(t, ok)
	require.Equal(t, []byte{1, 2, 3}, buf)

	require.True(t, m.WriteUint64LeAt(8, math.MaxUint64))
	v, ok := m.ReadUint64LeAt(8)
	require.True(t, ok)
	require.Equal(t, uint64(math.MaxUint64), v)

	require.True(t, m.WriteUint32LeAt(16, 0xdeadbeef))
	v32, ok := m.ReadUint32LeAt(16)
	require.True(t, ok)
	require.Equal(t, uint32(0xdeadbeef), v32)

	require.True(t, m.WriteUint16LeAt(20, 0xbeef))
	v16, ok := m.ReadUint16LeAt(20)
	require.True(t, ok)
	require.Equal(t, uint16(0xbeef), v16)

	require.True(t, m.WriteByteAt(22, 7))
	v8, ok := m.ReadByteAt(22)
	require.True(t, ok)
	require.Equal(t, byte(7), v8)

	t.Run("an offset past the end is refused", func(t *testing.T) {
		_, ok := m.Read64(MemoryPagesToBytesNum(1), 1)
		require.False(t, ok)
		require.False(t, m.Write64(MemoryPagesToBytesNum(1), []byte{0}))
		require.False(t, m.WriteString64(MemoryPagesToBytesNum(1), "x"))
	})
	t.Run("an offset whose end wraps a uint64 is refused", func(t *testing.T) {
		// offset+byteCount would wrap to 0, which is in bounds if the addition
		// is not checked for carry.
		_, ok := m.Read64(math.MaxUint64, 1)
		require.False(t, ok)
		_, ok = m.ReadUint64LeAt(math.MaxUint64 - 3)
		require.False(t, ok)
		require.False(t, m.WriteUint64LeAt(math.MaxUint64-3, 0))
		require.False(t, m.Write64(math.MaxUint64, []byte{0}))
		require.False(t, m.WriteByteAt(math.MaxUint64, 0))
		require.False(t, m.WriteUint16LeAt(math.MaxUint64, 0))
		require.False(t, m.WriteUint32LeAt(math.MaxUint64, 0))
		_, ok = m.ReadByteAt(math.MaxUint64)
		require.False(t, ok)
		_, ok = m.ReadUint16LeAt(math.MaxUint64)
		require.False(t, ok)
		_, ok = m.ReadUint32LeAt(math.MaxUint64)
		require.False(t, ok)
	})
}

func Test_rangeOutOfBounds(t *testing.T) {
	require.False(t, rangeOutOfBounds(0, 0, 0))
	require.False(t, rangeOutOfBounds(4, 4, 8))
	require.True(t, rangeOutOfBounds(4, 5, 8))
	// The sum wraps to 3, which would pass an unchecked comparison.
	require.True(t, rangeOutOfBounds(math.MaxUint64, 4, 8))
}

func TestTableInstance_Grow64(t *testing.T) {
	t.Run("a zero delta reports the current length", func(t *testing.T) {
		tbl := &TableInstance{References: make([]Reference, 3), IsTable64: true}
		require.Equal(t, uint64(3), tbl.Grow64(0, 0))
	})
	t.Run("growing succeeds and reports the previous length", func(t *testing.T) {
		max := uint64(10)
		tbl := &TableInstance{References: make([]Reference, 3), Max: &max, IsTable64: true}
		require.Equal(t, uint64(3), tbl.Grow64(2, 0xf))
		require.Equal(t, 5, len(tbl.References))
		require.Equal(t, Reference(0xf), tbl.References[4])
	})
	t.Run("a delta past the maximum fails with -1", func(t *testing.T) {
		max := uint64(4)
		tbl := &TableInstance{References: make([]Reference, 3), Max: &max, IsTable64: true}
		require.Equal(t, ^uint64(0), tbl.Grow64(2, 0))
	})
	t.Run("a delta past a table's 32-bit length ceiling fails with -1", func(t *testing.T) {
		tbl := &TableInstance{References: make([]Reference, 1), IsTable64: true}
		require.Equal(t, ^uint64(0), tbl.Grow64(math.MaxUint32+1, 0))
		require.Equal(t, ^uint64(0), tbl.Grow64(math.MaxUint64, 0))
	})
}

func TestModule_buildTables_MinimumLimit(t *testing.T) {
	m := ModuleInstance{Tables: make([]*TableInstance, 1)}
	err := m.buildTables(&Module{
		TableSection: []Table{{Min: uint64(MaximumFunctionIndex) + 1, Type: RefTypeFuncref, IsTable64: true}},
	}, true)
	require.EqualError(t, err, "table[0] minimum of 134217729 entries exceeds the limit of 134217728")
}

func TestStore_memoryLimitPages(t *testing.T) {
	s := NewStore(api.CoreFeaturesV2, nil)
	require.Equal(t, uint64(MemoryLimitPages), s.memoryLimitPages(&Memory{}))
	require.Equal(t, uint64(MemoryLimitPages), s.memoryLimitPages(&Memory{IsMemory64: true}))

	s.MemoryLimitPages, s.Memory64LimitPages = 2, 3
	require.Equal(t, uint64(2), s.memoryLimitPages(&Memory{}))
	require.Equal(t, uint64(3), s.memoryLimitPages(&Memory{IsMemory64: true}))

	// A zero field means the default rather than "no pages allowed", so a Store
	// built without going through NewStore still works.
	s.MemoryLimitPages, s.Memory64LimitPages = 0, 0
	require.Equal(t, uint64(MemoryLimitPages), s.memoryLimitPages(&Memory{}))
	require.Equal(t, uint64(MemoryLimitPages), s.memoryLimitPages(&Memory{IsMemory64: true}))
}
