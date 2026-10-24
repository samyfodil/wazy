#!/bin/sh
# Generates testdata/oracle from the WIT inputs -- files, and directories
# with deps/ -- in testdata/wit-parser/ui and testdata/cases, using the
# wasm-tools on PATH (1.262.0; `make build.component_testdata` runs this), so
# the expectations come from wit-parser rather than from this package.
#
# For each input and each feature set (none, --features active,
# --all-features; wit-parser's own inputs skip "active", whose result it
# ships) it writes <name>.<set>.json, the resolved packages from
# `wasm-tools component wit --json`, or, when wit-parser rejects the input,
# <name>.<set>.err, its first error line.
set -eu
cd "$(dirname "$0")"
rm -rf oracle
mkdir oracle
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
      printf "%s\n" "$out" > "oracle/$n.$set.json"
    else
      printf "%s\n" "$(printf "%s\n" "$out" | sed -n "1s/^error: //p")" > "oracle/$n.$set.err"
    fi
  done
done
