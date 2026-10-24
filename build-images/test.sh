#!/bin/sh
# Builds (unless IMAGE is set), then tests one build image:
#   build-images/test.sh <lang>
#
# The image's sample/build.sh runs in the image as an unprivileged --user with
# the sample bind-mounted at /src, exactly as the README tells users to. Every
# component it writes to out/ is then
#   1. validated (wasm-tools validate) and checked for the expected world
#      export (wasm-tools component wit),
#   2. run under wazy built from this checkout, asserting the exact output:
#      stdout for out/command*.wasm, a GET's status and body for out/proxy*.wasm.
# WORLDS in <lang>/test.env names the worlds the sample must cover; both the
# plain and the .opt variant of each are required.
#
# Finally the image's uncompressed size is checked against <lang>/size.<arch>:
# more than 10% growth fails.
set -eu

lang=${1:?usage: test.sh <lang>}
root=$(cd "$(dirname "$0")/.." && pwd)
dir=$root/build-images/$lang
. "$dir/test.env"

image=${IMAGE:-}
if [ -z "$image" ]; then
	image=wazy-build-$lang:test
	docker build -t "$image" -f "$dir/Dockerfile" "$root/build-images"
fi

work=$(mktemp -d)
# Files the container wrote may belong to a remapped uid (rootless or
# userns-remap docker), so the container deletes them.
cleanup() {
	docker run --rm -v "$work":/src --user "$(id -u):$(id -g)" "$image" find /src -mindepth 1 -delete 2>/dev/null || true
	rm -rf "$work"
}
trap cleanup EXIT
cp -R "$dir/sample/." "$work/"
# A sample's wit/ directories hold only its own world; the WASI WIT goes in
# as their deps.
find "$work" -type d -name wit | while read -r w; do
	cp -R "$root/build-images/wit/deps" "$w/deps"
done
chmod -R a+rwX "$work"

echo "--- build samples"
docker run --rm -v "$work":/src -w /src --user "$(id -u):$(id -g)" "$image" sh -c 'sh build.sh && chmod -R a+rX out'

echo "--- build runner"
(cd "$root" && go build -o "$work/.runner" ./build-images/runner)

wt() { docker run --rm -v "$work":/src -w /src --user "$(id -u):$(id -g)" "$image" wasm-tools "$@"; }

fail=0
check() { # world file export-prefix expected
	world=$1 f=$2 export=$3 want=$4
	if [ ! -f "$work/out/$f" ]; then
		echo "FAIL $f: not built"; fail=1; return
	fi
	if ! wt validate "out/$f"; then
		echo "FAIL $f: wasm-tools validate"; fail=1; return
	fi
	if ! wt component wit "out/$f" | grep -q "export $export@"; then
		echo "FAIL $f: world does not export $export"; fail=1; return
	fi
	if [ "$world" = command ]; then
		got=$("$work/.runner" run "$work/out/$f") || { echo "FAIL $f: run exited $?"; fail=1; return; }
	else
		got=$("$work/.runner" http "$work/out/$f" /) || { echo "FAIL $f: serve exited $?"; fail=1; return; }
	fi
	if [ "$got" != "$want" ]; then
		printf 'FAIL %s: output\n got: %s\nwant: %s\n' "$f" "$got" "$want"; fail=1; return
	fi
	echo "ok   $f ($(wc -c <"$work/out/$f") bytes)"
}

echo "--- test components"
for world in $WORLDS; do
	for f in $world.wasm $world.opt.wasm; do
		case $world in
		command) check command "$f" wasi:cli/run "hello from $lang" ;;
		proxy) check proxy "$f" wasi:http/incoming-handler "$(printf '200\nhello from %s' "$lang")" ;;
		esac
	done
done

echo "--- image size"
arch=$(docker image inspect -f '{{.Architecture}}' "$image")
size=$(docker image inspect -f '{{.Size}}' "$image")
echo "$image $arch: $size bytes uncompressed"
if [ -f "$dir/size.$arch" ]; then
	recorded=$(cat "$dir/size.$arch")
	if [ "$size" -gt $((recorded + recorded / 10)) ]; then
		echo "FAIL image grew more than 10% over recorded $recorded bytes ($dir/size.$arch)"; fail=1
	fi
else
	echo "warning: no recorded size in $dir/size.$arch"
fi

exit $fail
