#!/usr/bin/env bash
# Cold-start comparison: `wasmtime run <component>` vs the wazy coldstart binary
# on the identical wasip2 command component. Reports min/median/mean wall-clock
# (min = the least-contended, fairest single number) over N process launches,
# timed with perf_counter (ms precision -- /usr/bin/time's %e is only 10ms).
#
# wasmtime runs with --disable-cache for a TRUE cold start: wazy recompiles the
# component every process (no on-disk cache), so disabling wasmtime's persistent
# artifact cache keeps both apples-to-apples. (For this tiny component the cache
# lookup actually costs MORE than recompiling, so --disable-cache is also
# wasmtime's faster path here.)
# Usage: run.sh <wazy-coldstart-bin> <component.wasm> [runs]
set -euo pipefail
WAZY_BIN=$1
COMPONENT=$2
RUNS=${3:-50}

hi() { # $1=runs, $2..=command -> "min=.. median=.. mean=.."
  python3 -c '
import subprocess,time,sys
n=int(sys.argv[1]); cmd=sys.argv[2:]; ts=[]
for _ in range(n):
    s=time.perf_counter()
    subprocess.run(cmd,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    ts.append(time.perf_counter()-s)
ts.sort()
print(f"min={ts[0]*1000:.1f}ms median={ts[n//2]*1000:.1f}ms mean={sum(ts)/n*1000:.1f}ms (n={n})")' "$@"
}

echo "component: $COMPONENT  runs: $RUNS"
printf 'wasmtime '; hi "$RUNS" wasmtime run --disable-cache "$COMPONENT"
printf 'wazy     '; hi "$RUNS" "$WAZY_BIN" "$COMPONENT"
