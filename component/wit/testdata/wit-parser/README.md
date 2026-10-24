# wit-parser UI tests

`ui/` is the test inputs and expected results of wit-parser, the Bytecode
Alliance's reference WIT resolver, at
[bytecodealliance/wasm-tools@2e6b2d6d](https://github.com/bytecodealliance/wasm-tools/tree/2e6b2d6db499501a91a0b88a848435345bef1f66/crates/wit-parser/tests/ui)
(`crates/wit-parser/tests/ui/`). It is not committed: `make
build.component_testdata` fetches it at that commit, the pin in the Makefile,
and generates `../oracle/` from it (see `../gen-oracle.sh`).

- `ui/<name>.wit`, or a directory `ui/<name>/` with its `deps/`, with
  `ui/<name>.wit.json`: an input and wit-parser's resolved packages for it,
  with the `active` feature enabled (as its own test runner does).
- `ui/parse-fail/*.wit` with `*.wit.result`: an input wit-parser rejects, and
  its error.

`oracle_test.go` parses each input and compares the outcome with these, and,
under the other feature sets (none, and all), with what wit-parser makes of it
there, kept in `../oracle/`. An error must carry wit-parser's message (its
first line, without the context its test runner wraps a directory's error in).

`parse-fail/` has three `.wit.result` files whose inputs wit-parser no longer
has (`async`, `async1`, `unterminated-string`); nothing runs them.

One input is skipped, with its reason in `oracle_test.go`: `kinds-of-deps`,
whose `deps/` holds WIT packages encoded as components, in the binary
(`e.wasm`) and the text (`d.wat`) format, which this package does not read
(it reads `.wit` deps only). `simple-wasm-text.wat` is such a package too;
the test reads `.wit` inputs only.

To update, bump `wasm_tools_tests_version` in the Makefile, run `make
build.component_testdata`, and re-run the tests.
