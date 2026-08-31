// A standard-Go (not TinyGo) wasip1 guest that writes a file through
// os.File.Write and reads it back. Componentized with the preview1 adapter, its
// write goes through wasi:io/streams check-write + write -- the path Rust's
// fs::write (blocking-write-and-flush) never touches, which is why 35 Rust
// fixtures all passed while a budget that narrowed to zero silently wrote
// nothing. See wasiMaxWriteBudget in imports/wasip2/wasi_http.go.
package main

import (
	"fmt"
	"os"
)

func main() {
	// Sizes that bracket the adapter's buffering behaviour.
	for _, n := range []int{1, 8, 4096, 70000} {
		want := make([]byte, n)
		for i := range want {
			want[i] = byte('a' + i%26)
		}
		path := fmt.Sprintf("/w%d.bin", n)
		f, err := os.Create(path)
		if err != nil {
			fmt.Printf("size=%d create: %v\n", n, err)
			continue
		}
		nw, err := f.Write(want)
		cerr := f.Close()
		if err != nil || nw != n || cerr != nil {
			fmt.Printf("size=%d write: n=%d err=%v close=%v\n", n, nw, err, cerr)
			continue
		}
		got, err := os.ReadFile(path)
		if err != nil {
			fmt.Printf("size=%d readback: %v\n", n, err)
			continue
		}
		if len(got) != n {
			fmt.Printf("size=%d readback: len=%d want %d\n", n, len(got), n)
			continue
		}
		fmt.Printf("size=%d ok\n", n)
	}
}
