// Package abi is the Canonical ABI, written out by hand.
//
// Standard Go has no WIT bindings generator, so everything `wazy:compose/greeter`
// lowers to lives here: an allocator the host can call, and load/store for the
// two shapes the interface uses -- `string` and `list<string>`.
//
// Memory layout, straight from the Canonical ABI:
//
//	string        pointer + byte length, two i32s
//	list<string>  pointer + element count, two i32s, the pointed-at array
//	              being count elements of 8 bytes each -- a string pair
//	              per element -- aligned to 4
//
// A record is not in that list because it never reaches memory here: `visitor`
// has three flat fields (string is two, u32 is one), which is under the
// Canonical ABI's 16-value limit, so it is passed as three bare i32 parameters
// and never laid out.
package abi

import (
	"encoding/binary"
	"unsafe"
)

// arena is the only memory the host is given pointers into, and it deliberately
// is not the Go heap. The preview1 adapter calls cabi_realloc from inside a WASI
// call the Go runtime is already making, and allocating there re-enters the
// runtime on its own system stack -- "fatal: systemstack called from unexpected
// goroutine", which surfaces as an unreadable trap during _initialize. Bumping a
// pointer through a plain array touches no runtime machinery at all.
//
// The consumer re-enters it a second way: the host lowers the greeter's answers
// back into this memory *while* run is still on the stack. A bump pointer is
// re-entrant by construction, so that costs nothing either.
//
// The adapter's own state is the first 128 KiB. The rest is one call's worth of
// strings, handed back by the post-return each time.
var arena [256 << 10]byte

// base is the linear-memory address of arena[0]. Every pointer the host holds
// came out of Alloc, so a host pointer is always base plus an index into arena
// -- which is why nothing below has to do pointer arithmetic.
func base() uint32 { return uint32(uintptr(unsafe.Pointer(&arena[0]))) }

var (
	next  uint32 // bytes of arena handed out
	floor uint32 // where next settles once the runtime and adapter have started
)

// init runs at the tail of _initialize, by which point the adapter has taken its
// one permanent allocation. Everything above this mark belongs to a single call,
// and Reset gives it back.
func init() { floor = next }

// Reset returns the arena to the post-startup mark. Call it from the
// post-return: by then the host has copied every string out.
func Reset() { next = floor }

// Alloc bump-allocates size bytes at the given alignment and returns the
// linear-memory address. A zero size is legal and still returns an aligned
// address -- an empty list has to point somewhere.
func Alloc(size, align uint32) uint32 {
	if align == 0 {
		align = 1
	}
	addr := (base() + next + align - 1) &^ (align - 1)
	if next = addr - base() + size; next > uint32(len(arena)) {
		panic("compose/go: arena exhausted")
	}
	return addr
}

// bytes views size bytes of linear memory at a host pointer. Anything outside
// the arena is a lowering bug somewhere, and is worth a trap rather than a
// silently wrong string: the pointer subtraction wraps for ptr < base, so the
// single upper-bound test catches both ends.
func bytes(ptr, size uint32) []byte {
	if size == 0 {
		return nil
	}
	off := ptr - base()
	if off > uint32(len(arena)) || size > uint32(len(arena))-off {
		panic("compose/go: pointer outside the arena")
	}
	return arena[off : off+size : off+size]
}

//go:wasmexport cabi_realloc
func cabiRealloc(oldPtr, oldSize, align, newSize uint32) uint32 {
	ptr := Alloc(newSize, align)
	if oldSize > 0 && newSize > 0 {
		n := oldSize
		if newSize < n {
			n = newSize
		}
		copy(bytes(ptr, n), bytes(oldPtr, n))
	}
	return ptr
}

// LoadU32 reads one i32 out of linear memory.
func LoadU32(ptr uint32) uint32 { return binary.LittleEndian.Uint32(bytes(ptr, 4)) }

// LoadString copies a string out of linear memory and onto the Go heap, so it
// outlives the next Reset.
func LoadString(ptr, size uint32) string { return string(bytes(ptr, size)) }

// StoreString copies a Go string into the arena and returns the pair the ABI
// wants: address and byte length.
func StoreString(s string) (ptr, size uint32) {
	size = uint32(len(s))
	ptr = Alloc(size, 1)
	copy(bytes(ptr, size), s)
	return ptr, size
}

// LoadStringList copies a list<string> out of linear memory. count of zero
// reads nothing at all -- the pointer for an empty list is allowed to be any
// aligned address, so it must not be dereferenced.
func LoadStringList(ptr, count uint32) []string {
	if count == 0 {
		return nil
	}
	elems := bytes(ptr, count*8)
	out := make([]string, count)
	for i := range out {
		e := elems[i*8:]
		out[i] = LoadString(binary.LittleEndian.Uint32(e), binary.LittleEndian.Uint32(e[4:]))
	}
	return out
}

// StoreStringList writes a list<string> into the arena and returns the pair the
// ABI wants: address of the element array and element count. An empty list
// still gets a real, 4-aligned address, which is what the Canonical ABI asks
// for -- it just has no elements after it.
func StoreStringList(xs []string) (ptr, count uint32) {
	count = uint32(len(xs))
	ptr = Alloc(count*8, 4)
	for i, s := range xs {
		sp, sn := StoreString(s)
		// Re-view after StoreString: it bumped the arena, and the slice is
		// re-derived rather than held, so nothing can dangle.
		e := bytes(ptr+uint32(i)*8, 8)
		binary.LittleEndian.PutUint32(e, sp)
		binary.LittleEndian.PutUint32(e[4:], sn)
	}
	return ptr, count
}

// Pair writes the two i32s of a string or a list into a fresh 4-aligned cell
// and returns its address. Both worlds here return something that flattens to
// two values, which is over the Canonical ABI's single-value return limit, so
// the core function returns the address of the pair instead of the pair.
func Pair(a, b uint32) uint32 {
	ptr := Alloc(8, 4)
	cell := bytes(ptr, 8)
	binary.LittleEndian.PutUint32(cell, a)
	binary.LittleEndian.PutUint32(cell[4:], b)
	return ptr
}
