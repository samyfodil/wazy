# Async/CM perf profiling findings (perf/component-model-async)

Baseline commit acf639d. Machine heavily loaded during profiling → **ns/op is unreliable;
allocs/op + B/op are exact (load-independent) and are the metric used here.** CPU profiling
under load 36 gave only ~20ms of usable samples (dominated by the legit callback loop +
guest CallWithStack, no wazy-side time sink) — revisit CPU profiling when the box is quiet.

## Per-call async hot path
`BenchmarkCallAsyncFirstLight` (no-suspension callback call): **4 allocs / 390 B**
`BenchmarkCallAsyncAwaitImport` (full WAIT path): **11 allocs / 973 B** (2 of the 11 are
benchmark-harness closures, not runtime → ~9 runtime).

### Tranche 1 (shared per-call, in progress — task #17)
- `async_lift.go:84-85` — `&task{}` + `&guestTask{}` always paired → **co-allocate (2→1)**
- `guest_task.go:239` — `gt.firstRunBody` bound-method value escapes into `runSegment(fn)` → **−1**
- `async_builtins.go:571` — `[]abi.Value{val}` task.return 1-elem slice → **inline [1]abi.Value (−1)**
  → target FirstLight 4→1, AwaitImport 11→8.

### Tranche 1.5 (WAIT-path closures → methods; safe, load-independent)
- `async_host_import.go:298` — `st.applyResolve = func(vals) error {...}` captures st → make it a
  method + store captured data as `subtask` fields (−1)
- `async_host_import.go:159` — `st.setPendingEvent(func() eventTuple {...})` captures st → ditto (−1)

### Tranche 2 (WAIT-path struct pooling; needs reset-on-resolve design, higher risk — CM11 pattern)
- `async_host_import.go:445` — `&AsyncCall{...}` per WAIT
- `async_host_import.go:283` — `&subtask{...}` per WAIT (newSubtask; the `lenders: []*resourceEntry{}`
  empty-literal may also be avoidable — start nil, grow on first lend)
- `async_builtins.go:149` — `&waitableSet{}` per WAIT (waitableSetNew)
- Pool the co-allocated task/guestTask frame from Tranche 1 too → amortized ~0.

## Separate area: component Instantiate (higher volume for multi-instantiate workloads)
`BenchmarkInstantiateCached`: **98 allocs / 24.5 KB**; `BenchmarkInstantiateHelloCached`:
**1665 allocs / 368 KB**. Top alloc sites:
- `instantiateGraph` (the CM instantiation driver, 92% cum)
- engine `FunctionInstanceReference` (11%) + `NewFunction` (5%)
- `buildInstanceExportIndex` (map per instantiate)
- config churn: `newConfig` + `moduleConfig.clone` + `NewModuleConfig` (~3 allocs/instantiate for config objects — candidate to reuse/avoid the clone)
- core `Store.instantiate` / `buildGlobals` / `evaluateConstExpr` (partly engine, not CM-specific)
Diffuse; lower priority than the per-call path but worth a pass for embedders instantiating many
components.

## Coverage gap
No benchmark exercises the thread.* path (thread.yield / new-indirect / yield-then-resume). Add one
before optimizing that path so its allocs are measurable.
