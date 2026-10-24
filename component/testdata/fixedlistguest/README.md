# fixedlistguest

The guest `fixedlist_guest_test.go` runs: a Rust `wasm32-wasip2` component
built with `wit-bindgen` 0.63.0 against `wit/fixed.wit`, whose exports pass
`list<u32, 4>`, `list<string, 2>`, `list<point, 2>`,
`list<list<u8, 3>, 2>` and `list<u64, 20>` values, the first three through
host imports declared with those types too (strings only as an import's
result: wit-bindgen 0.63.0's Rust generator cannot pass a `list<string, N>`
to an import -- the code it generates moves out of the array).

Rebuild with `build.sh` in the pinned build image (see the script);
`target/` is not checked in, `../fixedlistguest.wasm` is what the test
embeds. wit-bindgen's C generator does not do fixed-length lists yet, and
its C++ one does not compile a `list<string, N>`, so the guest is Rust.
