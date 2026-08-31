# real_gowrite fixture

The source for `../real_gowrite.component.wasm`, kept so the fixture can be
rebuilt. It is a **standard Go** (not TinyGo) wasip1 command, componentized with
the upstream `wasi_snapshot_preview1` **command** adapter:

```sh
GOOS=wasip1 GOARCH=wasm go build -o core.wasm .
wasm-tools component new core.wasm \
    --adapt wasi_snapshot_preview1=wasi_snapshot_preview1.command.wasm \
    -o comp.wasm
wasm-tools strip comp.wasm -o ../real_gowrite.component.wasm
```

Why standard Go and not Rust, when every other fixture here is Rust: Go's
`internal/poll.FD.Write` reaches `wasi:io/streams` **check-write** and then
`write`, while Rust's `fs::write` uses `blocking-write-and-flush` and never
calls check-write at all. A check-write budget that was nonzero as a u64 but
zero once narrowed to a wasm32 `usize` therefore passed all 35 Rust fixtures
while writing nothing through this path. See `wasiMaxWriteBudget`.

It is the largest fixture in the tree (~2.7 MB) because a standard-Go binary
embeds the Go runtime; that is inherent to what it covers.
