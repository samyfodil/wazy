// The `greeter` world of greet.wit, in standard Go.
//
// Standard Go has no WIT bindings generator, so the Canonical ABI that
// `greet: func(name: string) -> string` lowers to is written out here by hand.
// It comes to three core exports:
//
//	greet(ptr, len i32) -> i32   the name arrives as (pointer, length) into our
//	                             memory; the answer goes back as the address of
//	                             a [pointer, length] pair
//	cabi_realloc(...)   -> i32   how the host allocates inside our memory
//	cabi_post_greet(i32)         runs once the host has copied the answer out
//
// `wasm-tools component embed` staples greet.wit onto the core module, and
// `wasm-tools component new` uses it to wrap those three functions into the
// typed component export a host calls as plain greet("wazy"). See README.md.
package main

import "unsafe"

// main never runs. -buildmode=c-shared makes this a reactor: the module exports
// `_initialize` instead of `_start`. Go still needs a main to link.
func main() {}

// arena is the only memory the host is given pointers into, and it deliberately
// is not the Go heap. The preview1 adapter calls cabi_realloc from inside a WASI
// call the Go runtime is already making, and allocating there re-enters the
// runtime on its own system stack -- "fatal: systemstack called from unexpected
// goroutine", which surfaces as an unreadable trap during _initialize. Bumping a
// pointer through a plain array touches no runtime machinery at all.
//
// The adapter's own state is the first 128 KiB of it; the rest is one call's
// worth of strings, handed back by cabi_post_greet each time.
var arena [192 << 10]byte

// base is the linear-memory address of arena[0]. Every pointer the host holds
// came out of alloc, so a host pointer is always base plus an index into arena
// -- which is why nothing below has to do pointer arithmetic.
func base() uint32 { return uint32(uintptr(unsafe.Pointer(&arena[0]))) }

var (
	next  uint32 // bytes of arena handed out
	floor uint32 // where next settles once the runtime and adapter have started
)

// init runs at the tail of _initialize, by which point the adapter has taken its
// one permanent allocation. Everything above this mark belongs to a single call,
// and cabi_post_greet gives it back.
func init() { floor = next }

func alloc(size, align uint32) uint32 {
	addr := (base() + next + align - 1) &^ (align - 1)
	if next = addr - base() + size; next > uint32(len(arena)) {
		panic("greeter: arena exhausted")
	}
	return addr
}

// at views size bytes of linear memory at a host pointer.
func at(ptr, size uint32) []byte {
	off := ptr - base()
	return arena[off : off+size]
}

//go:wasmexport cabi_realloc
func cabiRealloc(oldPtr, oldSize, align, newSize uint32) uint32 {
	if align == 0 {
		align = 1
	}
	ptr := alloc(newSize, align)
	if oldSize > 0 {
		copy(at(ptr, newSize), at(oldPtr, oldSize))
	}
	return ptr
}

// ret is greet's return area: a string comes back as the pair
// [pointer, length], and greet's i32 result is the address of that pair.
var ret [2]uint32

//go:wasmexport greet
func greet(ptr, size uint32) uint32 {
	answer := "Hello, " + string(at(ptr, size)) + "! (from Go)"

	out := alloc(uint32(len(answer)), 1)
	copy(at(out, uint32(len(answer))), answer)

	ret[0], ret[1] = out, uint32(len(answer))
	return uint32(uintptr(unsafe.Pointer(&ret)))
}

//go:wasmexport cabi_post_greet
func cabiPostGreet(uint32) { next = floor }
