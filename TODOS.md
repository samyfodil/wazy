# TODOS

## WASI 0.3 / async component-model follow-on
- **What:** Add the native async ABI on top of the real WASI 0.2 Component Model runtime: `future<T>`/`stream<T>`, task/subtask lifecycle, a host event loop, and reworked async interfaces (`wasi:io` is deleted in 0.3, folded into the Canonical ABI).
- **Why:** Would make wazy the only pure-Go runtime that can run async 0.3 components. Go's goroutines+channels back futures/streams more naturally than Wasmtime's Rust event loop — the one place wazy's substrate is an asset.
- **Context:** ~8–10k LOC on top of the p2 runtime. Reference: Wasmtime + `bytecodealliance/wasip3-prototyping`, and the async section of the component-model `definitions.py`. Zero pure-Go prior art. Highest-variance part is async correctness debugging.
- **Depends on / blocked by:** p2 CM runtime shipped and solid (milestones 1–4). Also blocked on the 0.3 spec settling — as of 2026-07 Wasmtime still marks its p3 support experimental/unstable. Do NOT start early; spec churn will waste the work.

## AliasDef must retain the core-sort discriminator
- **What:** `decodeAliasSection` in `internal/component/binary/decoder.go` parses past the one-byte core:sort discriminator (func/table/mem/global) on a core-export alias but doesn't store it on `AliasDef`. The instance engine currently assumes every core-export alias is a func alias.
- **Why:** True for every component the current milestone can instantiate (canon lift only references core funcs), but breaks the moment a component mixes core func aliases with core memory/table/global aliases — the engine would misresolve the core-func index space.
- **Context:** Add a `CoreSort byte` field to `AliasDef`, populate it in `decodeAliasSection`, and have `internal/component/instance` filter aliases by CoreSort==func when building the core-func index space. Surfaced during M3.2.
- **Depends on / blocked by:** Do before a milestone that instantiates a component with an exported memory/table (i.e. real WASI guests that use `canon lower` with a memory option).

## readResourcetypeDesc mislabels the rep valtype
- **What:** `binary/descriptor.go` `readResourcetypeDesc` decodes a resource's `rep` (a core type, `i32` = byte 0x7f) through the component-level primitive table, where 0x7f means `bool` — so `ResourceDesc.Rep` prints as "bool".
- **Why:** harmless today (the handle table and resource canons treat reps as raw uint32 at the ABI boundary and never read `ResourceDesc.Rep`), but wrong and will mislead anything that inspects resource rep types semantically.
- **Context:** the resourcetype rep is a core valtype, not a component valtype — decode it with the core-type reader. Surfaced during M4.1.
- **Depends on / blocked by:** none; low priority.

## Audit inherited interface bugs (http body, fs/sockets)
- **What:** During the interface-salvage lane, give the parts carried over from `wazero-wasip2` a dedicated correctness audit: the known http-body-loss bug, and the filesystem + sockets interfaces their README flags as un-reviewed (esp. resource release).
- **Why:** These are silent data bugs (dropped response bodies, leaked/mis-released handles) that won't surface in a hello-world demo but will bite real guests in production.
- **Context:** `wazero-wasip2` README lists: "HTTP has a chance of losing the body" and that only `clock/http/io/tls/random` were partially reviewed — filesystem and sockets unaudited. When salvaging syscall bodies, treat these as suspect and add targeted tests (body round-trip, handle-release leak assert).
- **Depends on / blocked by:** Lane E (interface implementation). Validate against the vendored WASI `.wit` as the surface oracle.
