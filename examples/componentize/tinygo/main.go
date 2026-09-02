// Command greet is the TinyGo implementation of the `wazy:examples/greeter`
// world: it exports one function, `greet(name: string) -> string`.
//
// The bindings under gen/ are generated from greet.wit by wit-bindgen-go; all
// this file does is fill in the exported function. See README.md to rebuild.
package main

import greeter "github.com/samyfodil/wazy/examples/componentize/tinygo/gen/wazy/examples/greeter"

func init() {
	greeter.Exports.Greet = func(name string) string {
		return "Hello, " + name + "! (from TinyGo)"
	}
}

// main is required by the Go toolchain but never runs: a component has no
// `_start`, only the exports it declares in its world.
func main() {}
