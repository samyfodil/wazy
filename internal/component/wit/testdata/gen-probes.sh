#!/bin/sh
# Regenerates testdata/probes.txt: what wit-parser (wasm-tools 1.262.0, in a
# pinned build image) makes of each testdata/probes/*.wit -- "name|OK", or
# "name|error|file:line:col", its error's first line and position -- so the
# lexer and parser tests check against wit-parser rather than this package.
# The container only reads; the host writes the file.
set -eu
cd "$(dirname "$0")"
IMAGE=ghcr.io/samyfodil/wazy-build-c:34.0
docker run --rm -v "$PWD/probes":/td:ro -w /td "$IMAGE" sh -eu -c '
for f in *.wit; do
  n=$(basename "$f" .wit)
  if wasm-tools component wit "$f" >/dev/null 2>/tmp/err; then
    printf "%s|OK\n" "$n"
  else
    printf "%s|%s|%s\n" "$n" "$(sed -n "1s/^error: //p" /tmp/err)" "$(sed -n "2s/^ *--> //p" /tmp/err)"
  fi
done
' > probes.txt

# The same for each source in mutations.txt (see gen-mutations.py), into
# mutations-result.txt, each source written as m.wit.
docker run --rm -v "$PWD":/td:ro -w /tmp "$IMAGE" sh -eu -c '
while IFS="|" read -r n src; do
  printf "%s" "$src" | base64 -d > m.wit
  if wasm-tools component wit m.wit >/dev/null 2>/tmp/err; then
    printf "%s|OK\n" "$n"
  else
    printf "%s|%s|%s\n" "$n" "$(sed -n "1s/^error: //p" /tmp/err)" "$(sed -n "2s/^ *--> //p" /tmp/err)"
  fi
done < /td/mutations.txt
' > mutations-result.txt
