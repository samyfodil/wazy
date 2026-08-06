// Control for the Windows corruption investigation (TODOS.md "## OPEN BUG").
// Poisoned runners (Intel Xeon Platinum 8573C) corrupt memory during wazy's
// teardown at ~80% of launches, while every AMD EPYC runner is clean. This
// program contains no wazy at all. If it corrupts on a poisoned runner, the
// fault is the platform or the Go runtime rather than anything in this repo.
//
// Mode is chosen by argv[1]:
//
//	heap  - pointer-dense heap + forced GC. Establishes whether plain
//	        allocation corrupts. Measured 0/40 on three poisoned runners, so
//	        allocation alone is NOT enough.
//	sched - adds what wazy actually does that "heap" does not: many goroutines
//	        handing a baton over unbuffered channels (the async scheduler's
//	        shape), plus finalizers, plus the same GC pressure.
//
// Lives under .github/ deliberately: Go tooling ignores dot-prefixed
// directories, so this never enters the module build.
package main

import (
	"os"
	"runtime"
)

type node struct {
	next *node
	buf  []byte // 16-byte allocations: the size class the GC reported as a zombie
}

// heap builds a deep pointer chain, walks it, and forces a collection.
func heap() {
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

// baton mirrors wazy's exactly-one-runnable primitive: a goroutine parked on
// an unbuffered channel, resumed by whoever holds the baton, with a finalizer
// attached to a heap object it owns.
type baton struct {
	resume chan struct{}
	yield  chan struct{}
	owned  *node
}

func sched() {
	for round := 0; round < 400; round++ {
		bs := make([]*baton, 0, 64)
		for i := 0; i < 64; i++ {
			b := &baton{resume: make(chan struct{}), yield: make(chan struct{})}
			b.owned = &node{buf: make([]byte, 16)}
			runtime.SetFinalizer(b.owned, func(*node) {})
			go func(b *baton) {
				for range b.resume {
					var head *node
					for j := 0; j < 64; j++ {
						head = &node{next: head, buf: make([]byte, 16)}
					}
					if head == nil {
						panic("unreachable")
					}
					b.yield <- struct{}{}
				}
				close(b.yield)
			}(b)
			bs = append(bs, b)
		}
		for pass := 0; pass < 4; pass++ {
			for _, b := range bs {
				b.resume <- struct{}{}
				<-b.yield
			}
		}
		for _, b := range bs {
			close(b.resume)
			<-b.yield // drain the close
		}
		runtime.GC()
	}
}

func main() {
	mode := "heap"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	switch mode {
	case "heap":
		heap()
	case "sched":
		sched()
	default:
		panic("unknown mode " + mode)
	}
}
