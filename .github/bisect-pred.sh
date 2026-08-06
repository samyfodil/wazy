#!/usr/bin/env bash
# `git bisect run` predicate for the Windows corruption (TODOS.md "## OPEN BUG").
# Only meaningful on a POISONED runner, where a bad commit reproduces at ~92%
# per launch; the caller probes for that before starting the bisect.
#
# Exit codes are git-bisect's: 0 good, 1 bad, 125 skip (does not build here).
#
# Copied to $RUNNER_TEMP before bisecting, because bisect checkouts would
# otherwise delete this file out from under the run.
set -u
RUN='TestAsyncWastConformance/trap-if-sync-and-waitable-set'
BIN="${RUNNER_TEMP:-/tmp}/bisect-t.exe"

go test -c -o "$BIN" ./internal/component/instance/ || exit 125

launches=25
crashes=0
for _ in $(seq 1 "$launches"); do
	set +e
	"$BIN" -test.run "$RUN" -test.count=1 -test.timeout=2m >/dev/null 2>&1
	rc=$?
	set -e
	[ "$rc" -ne 0 ] && crashes=$((crashes + 1))
done

echo "  $(git rev-parse --short HEAD): ${crashes}/${launches}"

# >=2 rather than >=1 so a single stray failure cannot mislabel a commit, while
# a genuinely bad commit measures ~23/25.
[ "$crashes" -ge 2 ] && exit 1
exit 0
