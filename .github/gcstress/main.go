// Control for the Windows corruption investigation (TODOS.md "## OPEN BUG").
// Poisoned runners corrupt memory during wazy's teardown; this program has no
// wazy in it at all, just a pointer-dense heap and forced GCs. If it also
// corrupts on a poisoned runner, the fault is the platform or the Go runtime
// rather than anything in this repository.
//
// Lives under .github/ deliberately: Go tooling ignores dot-prefixed
// directories, so this never enters the module build.
package main

import "runtime"

type node struct {
	next *node
	buf  []byte // 16-byte allocations: the size class the GC reported as a zombie
}

func main() {
	for round := 0; round < 200; round++ {
		var head *node
		for i := 0; i < 20000; i++ {
			head = &node{next: head, buf: make([]byte, 16)}
		}
		n := 0
		for c := head; c != nil; c = c.next {
			n += len(c.buf)
		}
		if n == 0 {
			panic("unreachable: the list was collected while still referenced")
		}
		runtime.GC()
	}
}
