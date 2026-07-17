#!/usr/bin/env bash
# Waits for a quiet machine (load < THRESHOLD and no test262.test), then runs
# both benchmark suites against wasmtime and writes results to OUT. Meant to be
# launched in the background so timing only happens once the box is idle.
set -uo pipefail
REPO=/home/samy/Documents/wazy
SCRATCH=/tmp/claude-1000/-home-samy-Documents-wazy/ff95bff2-9a88-454e-8ae3-d5505273e33c/scratchpad
OUT=$SCRATCH/bench-results.txt
COMPONENT=$REPO/internal/component/instance/testdata/real_hello.component.wasm
THRESHOLD=3.0

: > "$OUT"
# Wait until the 1-min load average drops below THRESHOLD and test262 is gone.
while :; do
  load=$(awk '{print $1}' /proc/loadavg)
  if ! pgrep -x test262.test >/dev/null && awk -v l="$load" -v t="$THRESHOLD" 'BEGIN{exit !(l<t)}'; then
    break
  fi
  sleep 30
done

{
  echo "=== quiet at $(uptime) ==="
  echo
  echo "### Component cold-start (wall clock, same component both arms)"
  go build -o "$SCRATCH/wazy-coldstart" "$REPO/benchmarks/coldstart" && \
    bash "$REPO/benchmarks/coldstart/run.sh" "$SCRATCH/wazy-coldstart" "$COMPONENT" 50
  echo
  echo "### Core-engine three-way (wazy / wazero / wasmtime, identical wasm)"
  cd "$REPO/benchmarks/vs-wazero" && \
    go test -run '^$' -bench 'Compile3|Execute3' -benchmem -count 5 . 2>&1
} >> "$OUT" 2>&1

echo "=== done at $(uptime) ===" >> "$OUT"
