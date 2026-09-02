// The `consumer` world of greeter.wit, in standard Go: it imports the
// wazy:compose/greeter interface -- satisfied at composition time by a provider
// in any of the six languages -- and exports `run`.
//
// The import side is the mirror image of the export side. A result that
// flattens to more than one value comes back through a return area the *caller*
// supplies, so each imported function takes one extra i32 and returns nothing:
//
//	greet(ptr, len, id, retptr)   retptr receives a string pair
//	greet-all(ptr, count, retptr) retptr receives a list pair
//
// Nothing below spells out a single character of the provider's wording. Every
// byte in run's answer came back across the interface, so a lift or lower that
// loses bytes, drops an element or misreads a length shows up in the output
// rather than hiding behind a matching constant.
package main

import (
	"strconv"

	"wazy.examples/compose/go/abi"
)

func main() {}

//go:wasmimport wazy:compose/greeter@0.1.0 greet
func importGreet(namePtr, nameLen, id, ret uint32)

//go:wasmimport wazy:compose/greeter@0.1.0 greet-all
func importGreetAll(namesPtr, namesLen, ret uint32)

// greet calls the imported `greet: func(who: visitor) -> string`. The record is
// passed flat: name pointer, name length, id.
func greet(name string, id uint32) string {
	namePtr, nameLen := abi.StoreString(name)
	ret := abi.Alloc(8, 4)
	importGreet(namePtr, nameLen, id, ret)
	return abi.LoadString(abi.LoadU32(ret), abi.LoadU32(ret+4))
}

// greetAll calls the imported `greet-all: func(names: list<string>) -> list<string>`.
// It returns the list as it actually came back -- length included -- so a
// truncated or padded answer is visible to the caller.
func greetAll(names []string) []string {
	namesPtr, namesLen := abi.StoreStringList(names)
	ret := abi.Alloc(8, 4)
	importGreetAll(namesPtr, namesLen, ret)
	return abi.LoadStringList(abi.LoadU32(ret), abi.LoadU32(ret+4))
}

//go:wasmexport run
func run() uint32 {
	out := make([]string, 3)

	// 1. a record with a string field, across the boundary and back.
	out[0] = greet("wazy", 42)

	// 2. list<string> both ways. Report what actually arrived rather than
	//    assuming two elements came back.
	pair := greetAll([]string{"a", "b"})
	if len(pair) > 0 {
		out[1] = pair[0]
	} else {
		out[1] = "greet-all([a,b]) returned len=" + strconv.Itoa(len(pair))
	}

	// 3. the empty-list path. The literal is the length the provider actually
	//    returned, so a wrong one is printed, not hidden.
	empty := greetAll(nil)
	out[2] = "empty-len=" + strconv.Itoa(len(empty))

	return abi.Pair(abi.StoreStringList(out))
}

//go:wasmexport cabi_post_run
func runPost(uint32) { abi.Reset() }
