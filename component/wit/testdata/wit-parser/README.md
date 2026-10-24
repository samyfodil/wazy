# wit-parser UI tests

Verbatim copies of test inputs and expected results from wit-parser, the
Bytecode Alliance's reference WIT resolver, at
[bytecodealliance/wasm-tools@2e6b2d6d](https://github.com/bytecodealliance/wasm-tools/tree/2e6b2d6db499501a91a0b88a848435345bef1f66/crates/wit-parser/tests/ui)
(`crates/wit-parser/tests/ui/`). Nothing here is generated or edited by wazy.

- `ui/<name>.wit`, or a directory `ui/<name>/` with its `deps/`, with
  `ui/<name>.wit.json`: an input and wit-parser's resolved packages for it,
  with the `active` feature enabled (as its own test runner does).
- `ui/parse-fail/*.wit` with `*.wit.result`: an input wit-parser rejects, and
  its error.

`oracle_test.go` parses each input and compares the outcome with these,
and, under the other feature sets (none, and all), with what wit-parser
makes of it there, kept in `../oracle/` (see `../gen-oracle.sh`).
wit-parser's single-file tests write dependencies as nested
`package x { ... }` blocks, which this package does not read; the test splits
those into a `deps/` directory, the layout `ParseFS` reads.

Not copied, because this package does not do what they test:

- WIT accessors (`prop: get() -> T;`, `accessors*.wit`,
  `include-accessor-pair.wit`, `parse-fail/accessor-*`).
- Directories whose files nest packages (`multi-file-multi-package`,
  `multi-package-*`) or whose deps are `.wasm`/`.wat` (`kinds-of-deps`).
- `parse-fail` inputs for checks this package does not make, which it
  accepts: names that differ only in case, duplicate fields, cases and
  params, empty enums, tuples and variants, more than 32 flags, a returned
  `borrow`, constructor result shapes, package and interface dependency
  cycles, and doc-comment and `@external-id` placement rules.
- The other `parse-fail` inputs, which this package rejects in words of its
  own; only those that bear on world items are copied, with their wording
  listed in `oracle_test.go`.

To update, re-copy the same files from a newer commit, run
`../gen-oracle.sh`, and re-run the tests.

Licensed Apache-2.0 WITH LLVM-exception, or Apache-2.0, or MIT, per
wasm-tools; see `LICENSE-Apache-2.0_WITH_LLVM-exception` and `LICENSE-MIT`.
