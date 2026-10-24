#!/bin/sh
# Regenerates testdata/oracle from the WIT inputs -- files, and directories
# with deps/ -- in testdata/wit-parser/ui and testdata/cases, using the wasm-tools in a pinned build image (1.262.0), so
# the expectations come from wit-parser rather than from this package.
#
# For each input and each feature set (none, --features active,
# --all-features; wit-parser's own inputs skip "active", whose result it
# ships) it writes <name>.<set>.json, the resolved packages from
# `wasm-tools component wit --json`, or, when wit-parser rejects the input,
# <name>.<set>.err, its first error line. The container only reads; the host
# writes the files.
set -eu
cd "$(dirname "$0")"
IMAGE=ghcr.io/samyfodil/wazy-build-c:34.0
rm -f oracle/*.json oracle/*.err
mkdir -p oracle
docker run --rm -v "$PWD":/td:ro -w /td "$IMAGE" sh -eu -c '
for f in wit-parser/ui/*.wit wit-parser/ui/*/ cases/*.wit; do
  [ -e "$f" ] || continue
  case $f in */parse-fail/) continue ;; esac
  n=$(basename "$f" .wit)
  sets="none active all"
  # wit-parser ships the "active" result for its own inputs.
  case $f in wit-parser/*) sets="none all" ;; esac
  for set in $sets; do
    case $set in
      none) flags= ;;
      active) flags="--features active" ;;
      all) flags=--all-features ;;
    esac
    if out=$(wasm-tools component wit $flags --json "$f" 2>&1); then
      printf "=== %s.%s.json\n%s\n" "$n" "$set" "$out"
    else
      printf "=== %s.%s.err\n%s\n" "$n" "$set" "$(printf "%s\n" "$out" | sed -n "1s/^error: //p")"
    fi
  done
done
' | awk '/^=== / { out = "oracle/" $2; next } { print > out }'
