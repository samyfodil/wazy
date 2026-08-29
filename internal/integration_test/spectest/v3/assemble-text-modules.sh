#!/bin/sh
# Make every command in the suite's JSON one that actually runs, so a pass means something.
#
# A "(module quote ...)" in a .wast carries its module as source text. `wasm-tools json-from-wast`
# writes those out as .wat and labels the command module_type "text" -- but where the text actually
# assembles it also writes the .wasm beside it. wazy loads binary modules only, so left alone every
# such command goes untested.
#
# So: where the .wasm exists, rewrite the command to name it. That turns an instantiation into a real
# instantiation and an assert_invalid into a real validation check. Only those two kinds are
# rewritten. An assert_malformed is asserting that the *text* is malformed, which no binary can
# express; where one of those assembles anyway, running it would assert that wazy rejects a module
# that is perfectly well-formed.
#
# The rest of the assert_malformed cases -- the great majority, and the ones with no .wasm at all --
# are asserting that a number literal, a token or a name is bad *as text*. There is nothing there for
# a runtime that reads binaries to check, and the harness would quietly report each as a pass without
# having run anything. Drop them instead, along with the two files that are nothing else, so that the
# count the suite reports is the count it verified.
#
# Usage: assemble-text-modules.sh <testdata-dir>
set -eu
cd "$1"

ls *.wasm >assembled.list
for j in *.json; do
	jq --rawfile a assembled.list '
		($a | split("\n") | map(select(length > 0))) as $ok
		| .commands |= map(
			((.filename // "") | sub("\\.wat$"; ".wasm")) as $wasm
			| if .module_type == "text"
				and (.type == "module" or .type == "assert_invalid")
				and ($ok | index($wasm))
			then .module_type = "binary" | .filename = $wasm
			else . end)
		| .commands |= map(select(.module_type != "text" or .type != "assert_malformed"))
	' "$j" >"$j.tmp"
	mv "$j.tmp" "$j"

	# A file of nothing but text-format assertions has nothing left to run.
	if [ "$(jq '.commands | length' "$j")" -eq 0 ]; then
		rm -f "$j" "$(basename "$j" .json).wast"
	fi
done

rm -f assembled.list *.wat
