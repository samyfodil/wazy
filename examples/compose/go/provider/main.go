// The `provider` world of greeter.wit, in standard Go: it exports the
// wazy:compose/greeter interface for a consumer in any language to call.
//
// Four core exports carry the interface, plus cabi_realloc from the abi
// package. The names are the Canonical ABI's, `interface#function`, and they
// go through //go:wasmexport verbatim -- Go does not mind the colon, slash or
// hash:
//
//	wazy:compose/greeter@0.1.0#greet          (ptr, len, id) -> pair address
//	wazy:compose/greeter@0.1.0#greet-all      (ptr, count)   -> pair address
//	cabi_post_wazy:compose/greeter@0.1.0#greet         releases the answer
//	cabi_post_wazy:compose/greeter@0.1.0#greet-all     releases the answers
//
// `wasm-tools component embed` staples greeter.wit onto the core module and
// `wasm-tools component new` wraps those into the typed component export.
// See ../README.md.
package main

import (
	"strconv"

	"wazy.examples/compose/go/abi"
)

// main never runs. -buildmode=c-shared makes this a reactor: the module exports
// `_initialize` instead of `_start`. Go still needs a main to link.
func main() {}

// greet is `greet: func(who: visitor) -> string`.
//
// `visitor` flattens to three i32s -- the two of its string field, then its u32
// -- so the record arrives unpacked and is never laid out in memory.
//
//go:wasmexport wazy:compose/greeter@0.1.0#greet
func greet(namePtr, nameLen, id uint32) uint32 {
	name := abi.LoadString(namePtr, nameLen)
	answer := "Hello, " + name + " #" + strconv.FormatUint(uint64(id), 10) + "! (from Go)"
	return abi.Pair(abi.StoreString(answer))
}

//go:wasmexport cabi_post_wazy:compose/greeter@0.1.0#greet
func greetPost(uint32) { abi.Reset() }

// greetAll is `greet-all: func(names: list<string>) -> list<string>`.
//
// An empty input is not special-cased: it loads as a nil slice, the loop runs
// zero times, and StoreStringList hands back a real address with a count of
// zero. Reading a list of length zero is the one thing that must not touch the
// incoming pointer, and LoadStringList doesn't.
//
//go:wasmexport wazy:compose/greeter@0.1.0#greet-all
func greetAll(namesPtr, namesLen uint32) uint32 {
	names := abi.LoadStringList(namesPtr, namesLen)
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = name + " (via Go)"
	}
	return abi.Pair(abi.StoreStringList(out))
}

//go:wasmexport cabi_post_wazy:compose/greeter@0.1.0#greet-all
func greetAllPost(uint32) { abi.Reset() }
