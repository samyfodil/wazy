# WASI 0.2 WIT

Verbatim WASI 0.2.12 interface definitions (the same copies as
`imports/wasip2/testdata/wasi-wit`), for samples that generate bindings for a
WASI world. A sample that needs them uses `wit/` with a world file of its own
and these files as `wit/deps/`; `test.sh` copies them in, and so should you
when you copy a sample out of this repo.
