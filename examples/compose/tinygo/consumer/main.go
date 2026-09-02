// Command consumer implements the `consumer` world of wazy:compose/greeter
// (see ../greeter.wit): it imports the interface and exports `run`.
//
// Nothing it returns is written here. Every one of the three strings comes back
// across the interface from whichever provider component it is composed with,
// so a wrong lift or lower shows up in the output instead of being masked.
package main

import (
	"strconv"

	"go.bytecodealliance.org/cm"

	consumer "github.com/samyfodil/wazy/examples/compose/tinygo/gen/consumer/wazy/compose/consumer"
	greeter "github.com/samyfodil/wazy/examples/compose/tinygo/gen/consumer/wazy/compose/greeter"
)

func init() {
	consumer.Exports.Run = func() cm.List[string] {
		out := make([]string, 3)

		// 1. a record with a string field, lowered into the provider's memory.
		out[0] = greeter.Greet(greeter.Visitor{Name: "wazy", ID: 42})

		// 2. list<string> both ways: two in, two out, first one reported.
		two := greeter.GreetAll(cm.ToList([]string{"a", "b"})).Slice()
		if len(two) == 2 {
			out[1] = two[0]
		} else {
			// Never silently pick element 0 of a list that came back the wrong
			// length -- report what actually arrived.
			out[1] = "greet-all-len=" + strconv.Itoa(len(two))
		}

		// 3. the empty-list path: zero in must mean zero out.
		none := greeter.GreetAll(cm.ToList([]string{}))
		out[2] = "empty-len=" + strconv.Itoa(int(none.Len()))

		return cm.ToList(out)
	}
}

// main is required by the Go toolchain but never runs: a component has no
// `_start`, only the exports it declares in its world.
func main() {}
