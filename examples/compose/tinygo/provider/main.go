// Command provider implements the `provider` world of wazy:compose/greeter
// (see ../greeter.wit): it exports the interface that a consumer component
// written in any other language can import.
//
// The bindings under ../gen/provider are generated from greeter.wit by
// wit-bindgen-go; all this file does is fill in the two exported functions.
// See ../README.md to rebuild.
package main

import (
	"strconv"

	"go.bytecodealliance.org/cm"

	greeter "github.com/samyfodil/wazy/examples/compose/tinygo/gen/provider/wazy/compose/greeter"
)

// lang is the one place the language name appears; both exports quote it so a
// composed pair makes it obvious which half produced which string.
const lang = "TinyGo"

func init() {
	greeter.Exports.Greet = func(who greeter.Visitor) string {
		return "Hello, " + who.Name + " #" + strconv.FormatUint(uint64(who.ID), 10) + "! (from " + lang + ")"
	}

	greeter.Exports.GreetAll = func(names cm.List[string]) cm.List[string] {
		// One element out per element in -- including none at all for an empty
		// input. The empty list is deliberately not special-cased: a zero-length
		// list<string> round trip is part of what this example tests.
		in := names.Slice()
		out := make([]string, len(in))
		for i, name := range in {
			out[i] = name + " (via " + lang + ")"
		}
		return cm.ToList(out)
	}
}

// main is required by the Go toolchain but never runs: a component has no
// `_start`, only the exports it declares in its world.
func main() {}
