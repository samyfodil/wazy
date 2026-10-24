
gofumpt       := mvdan.cc/gofumpt@v0.6.0
gosimports    := github.com/rinchsan/gosimports/cmd/gosimports@v0.3.8
golangci_lint := github.com/golangci/golangci-lint/cmd/golangci-lint@v1.64.5
asmfmt        := github.com/klauspost/asmfmt/cmd/asmfmt@v1.3.2

# Make 3.81 doesn't support '**' globbing: Set explicitly instead of recursion.
all_sources   := $(wildcard *.go */*.go */*/*.go */*/*/*.go */*/*/*.go */*/*/*/*.go)
all_testdata  := $(wildcard testdata/* */testdata/* */*/testdata/* */*/testdata/*/* */*/*/testdata/*)
all_testing   := $(wildcard internal/testing/* internal/testing/*/* internal/testing/*/*/*)
all_examples  := $(wildcard examples/* examples/*/* examples/*/*/* examples/*/*/*/* examples/*/*/*/*/* */*/example/* */*/example/*/* */*/example/*/*/*)
all_it        := $(wildcard internal/integration_test/* internal/integration_test/*/* internal/integration_test/*/*/*)
all_samples   := $(wildcard build-images/*/sample/*.go build-images/*/sample/*/*.go)
# main_sources exclude any test or example related code, and the build-image
# samples, which are their own modules
main_sources  := $(wildcard $(filter-out %_test.go benchmarks/% $(all_samples) $(all_testdata) $(all_testing) $(all_examples) $(all_it), $(all_sources)))
# main_packages collect the unique main source directories (sort will dedupe).
# Paths need to all start with ./, so we do that manually vs foreach which strips it.
main_packages := $(sort $(foreach f,$(dir $(main_sources)),$(if $(findstring ./,$(f)),./,./$(f))))

go_test_options ?= -timeout 300s

.PHONY: test.examples
test.examples:
	@go test $(go_test_options) ./examples/... ./imports/assemblyscript/example/... ./imports/emscripten/... ./imports/wasi_snapshot_preview1/example/...

.PHONY: build.examples.as
build.examples.as:
	@cd ./imports/assemblyscript/example/testdata && npm install && npm run build

%.wasm: %.zig
	@(cd $(@D); zig build -Doptimize=ReleaseSmall)
	@mv $(@D)/zig-out/*/$(@F) $(@D)

.PHONY: build.examples.zig
build.examples.zig: examples/allocation/zig/testdata/greet.wasm imports/wasi_snapshot_preview1/example/testdata/zig/cat.wasm imports/wasi_snapshot_preview1/testdata/zig/wasi.wasm
	@cd internal/testing/dwarftestdata/testdata/zig; zig build; mv zig-out/*/main.wasm ./ # Need DWARF custom sections.

tinygo_reactor_sources_reactor := examples/basic/testdata/add.go examples/allocation/tinygo/testdata/greet.go
.PHONY: build.examples.tinygo_reactor
build.examples.tinygo_reactor: $(tinygo_sources_reactor)
	@for f in $^; do \
	    tinygo build -o $$(echo $$f | sed -e 's/\.go/\.wasm/') -scheduler=none --no-debug --target=wasip1 -buildmode=c-shared $$f; \
	done

tinygo_sources_clis := examples/cli/testdata/cli.go imports/wasi_snapshot_preview1/example/testdata/tinygo/cat.go imports/wasi_snapshot_preview1/testdata/tinygo/wasi.go cmd/wazy/testdata/cat/cat.go
.PHONY: build.examples.tinygo_clis
build.examples.tinygo_clis: $(tinygo_sources_clis)
	@for f in $^; do \
	    tinygo build -o $$(echo $$f | sed -e 's/\.go/\.wasm/') -scheduler=none --no-debug --target=wasip1 $$f; \
	done
	@mv cmd/wazy/testdata/cat/cat.wasm cmd/wazy/testdata/cat/cat-tinygo.wasm

.PHONY: build.examples.tinygo
build.examples.tinygo: build.examples.tinygo_reactor build.examples.tinygo_clis

# We use zig to build C as it is easy to install and embeds a copy of zig-cc.
# Note: Don't use "-Oz" as that breaks our wasi sock example.
c_sources := imports/wasi_snapshot_preview1/example/testdata/zig-cc/cat.c imports/wasi_snapshot_preview1/testdata/zig-cc/wasi.c internal/testing/dwarftestdata/testdata/zig-cc/main.c
.PHONY: build.examples.zig-cc
build.examples.zig-cc: $(c_sources)
	@for f in $^; do \
	    zig cc --target=wasm32-wasi -o $$(echo $$f | sed -e 's/\.c/\.wasm/') $$f; \
	done

# Here are the emcc args we use:
#
# * `-Oz` - most optimization for code size.
# * `--profiling` - adds the name section.
# * `-s STANDALONE_WASM` - ensures wasm is built for a non-js runtime.
# * `-s EXPORTED_FUNCTIONS=_malloc,_free` - export allocation functions so that
#   they can be used externally as "malloc" and "free".
# * `-s WARN_ON_UNDEFINED_SYMBOLS=0` - imports not defined in JavaScript error
#   otherwise. See https://github.com/emscripten-core/emscripten/issues/13641
# * `-s TOTAL_STACK=8KB -s TOTAL_MEMORY=64KB` - reduce memory default from 16MB
#   to one page (64KB). To do this, we have to reduce the stack size.
# * `-s ALLOW_MEMORY_GROWTH` - allows "memory.grow" instructions to succeed, but
#   requires a function import "emscripten_notify_memory_growth".
emscripten_sources := $(wildcard imports/emscripten/testdata/*.cc)
.PHONY: build.examples.emscripten
build.examples.emscripten: $(emscripten_sources)
	@for f in $^; do \
		em++ -Oz --profiling \
		-s STANDALONE_WASM \
		-s EXPORTED_FUNCTIONS=_malloc,_free \
		-s WARN_ON_UNDEFINED_SYMBOLS=0 \
		-s TOTAL_STACK=8KB -s TOTAL_MEMORY=64KB \
		-s ALLOW_MEMORY_GROWTH \
		--std=c++17 -o $$(echo $$f | sed -e 's/\.cc/\.wasm/') $$f; \
	done

%/greet.wasm : cargo_target := wasm32-unknown-unknown
%/cat.wasm : cargo_target := wasm32-wasip1
%/wasi.wasm : cargo_target := wasm32-wasip1

.PHONY: build.examples.rust
build.examples.rust: examples/allocation/rust/testdata/greet.wasm imports/wasi_snapshot_preview1/example/testdata/cargo-wasi/cat.wasm imports/wasi_snapshot_preview1/testdata/cargo-wasi/wasi.wasm internal/testing/dwarftestdata/testdata/rust/main.wasm.xz

# Normally, we build release because it is smaller. Testing dwarf requires the debug build.
internal/testing/dwarftestdata/testdata/rust/main.wasm.xz:
	cd $(@D) && cargo build --target wasm32-wasip1
	mv $(@D)/target/wasm32-wasip1/debug/main.wasm $(@D)
	cd $(@D) && xz -k -f ./main.wasm # Rust's DWARF section is huge, so compress it.

# Builds rust using cargo normally
%.wasm: %.rs
	@(cd $(@D); cargo build --target $(cargo_target) --release)
	@mv $(@D)/target/$(cargo_target)/release/$(@F) $(@D)

spectest_base_dir := internal/integration_test/spectest

# GitHub's API allows 60 unauthenticated requests an hour per IP, which the shared CI runners burn
# through long before these targets have made their eleven. When the limit is hit the response is an
# error object rather than a list, jq fails mid-pipeline, and the download loop quietly produces
# nothing -- into a directory the target has already emptied, so the damage only surfaces much later
# as "pattern testdata/*.json: no matching files found". Send a token where one exists: the limit
# becomes 5000 and the flake goes away.
gh_api_curl := curl -fsSL --retry 5 --retry-all-errors $(if $(GITHUB_TOKEN),-H "Authorization: Bearer $(GITHUB_TOKEN)")

# The tools the fetched suites are built with, pinned like the suites themselves: another version can emit
# different bytes. Each is downloaded for this host into build/tools on first use, rather than taken from
# whatever happens to be on PATH.
wabt_version := 1.0.39
wasm_tools_version := 1.253.0
# Some component-model cases were written against a later wasm-tools than the rest reproduce under.
wasm_tools_next_version := 1.262.0
tools_dir := $(CURDIR)/build/tools
wast2json := $(tools_dir)/wabt-$(wabt_version)/wast2json
wasm_tools := $(tools_dir)/wasm-tools-$(wasm_tools_version)/wasm-tools
wasm_tools_next := $(tools_dir)/wasm-tools-$(wasm_tools_next_version)/wasm-tools
host_arm64 = $(filter arm64 aarch64,$(shell uname -m))
host_darwin = $(filter Darwin,$(shell uname -s))
wabt_release = wabt-$(wabt_version)-$(if $(host_darwin),macos,linux)-$(if $(host_arm64),arm64,x64)
wasm_tools_release = wasm-tools-$(1)-$(if $(host_arm64),aarch64,x86_64)-$(if $(host_darwin),macos,linux)

$(wast2json):
	@rm -rf $(@D) $(@D).tmp && mkdir -p $(@D).tmp
	@curl -fsSL --retry 5 --retry-all-errors \
		https://github.com/WebAssembly/wabt/releases/download/$(wabt_version)/$(wabt_release).tar.gz \
		| tar --strip-components 2 -C $(@D).tmp -xzf - wabt-$(wabt_version)/bin/wast2json
	@mv $(@D).tmp $(@D)

$(tools_dir)/wasm-tools-%/wasm-tools:
	@rm -rf $(@D) $(@D).tmp && mkdir -p $(@D).tmp
	@curl -fsSL --retry 5 --retry-all-errors \
		https://github.com/bytecodealliance/wasm-tools/releases/download/v$*/$(call wasm_tools_release,$*).tar.gz \
		| tar --strip-components 1 -C $(@D).tmp -xzf - $(call wasm_tools_release,$*)/wasm-tools
	@mv $(@D).tmp $(@D)

# Empties a fetched directory but for the .gitignore that keeps it in the tree: the tests embed the
# directory, so it has to exist whether or not anything has been fetched into it. Once a suite is built
# its .wast go too: the tests read the .json and .wasm, and the source would only bloat their binaries.
clean_fetched = find $(1) -mindepth 1 ! -name .gitignore -delete


# WebAssembly/testsuite as a submodule: the amalgamated core suite, pinned by gitlink rather than
# copied in. Suites generated from it keep no cases in the tree -- refreshing upstream is a submodule
# bump, not several thousand files of derived output in a diff.
spectest_testsuite_dir := $(spectest_base_dir)/testsuite

# Depend on a file inside the submodule, not the directory: a checkout that did not fetch submodules
# leaves the directory there but empty, and make would take that for a satisfied target.
$(spectest_testsuite_dir)/binary.wast:
	@git submodule update --init --depth 1 $(spectest_testsuite_dir)

spectest_v1_dir := $(spectest_base_dir)/v1
spectest_v1_testdata_dir := $(spectest_v1_dir)/testdata
spec_version_v1 := wg-1.0

spectest_v2_dir := $(spectest_base_dir)/v2
spectest_v2_testdata_dir := $(spectest_v2_dir)/testdata
spec_version_v2 := wg-2.0

spectest_v3_interaction_dir := $(spectest_base_dir)/v3-interaction
spectest_v3_interaction_testdata_dir := $(spectest_v3_interaction_dir)/testdata
# The cases crossing two of 3.0's proposals. The wasmtime-*.wast are Wasmtime's own tests, from its
# tests/misc_testsuite: an implementation's hand-written extras, not the specification's suite. They are
# fetched at this pin and not committed. The wazy-*.wast are this repository's, for the crossings those
# do not reach, and are committed along with what they generate.
wasmtime_version := d8a0da6d661605713798c1c9c76be5c28e3159ff
wasmtime_interaction_wast_files := \
	gc/alloc-v128-struct.wast gc/array-init-data.wast gc/array-new-data.wast \
	gc/array-new-elem.wast gc/const-expr-gc-simd.wast memory64/bounds.wast \
	memory64/multi-memory.wast memory64/offsets.wast memory64/simd.wast

spectest_v3_dir := $(spectest_base_dir)/v3
spectest_v3_testdata_dir := $(spectest_v3_dir)/testdata
# WebAssembly 3.0 has no test suite of its own in the spec repository: there,
# the eight proposals it folded in still sit in per-proposal subdirectories of
# test/core. WebAssembly/testsuite is the amalgamation -- its root is the core
# suite with every finished proposal merged in, and since 3.0 was released the
# only things left under proposals/ are the ones that came after it. So its
# root is the 3.0 corpus. The commit is pinned by the submodule, not here.

spectest_threads_dir := $(spectest_base_dir)/threads
spectest_threads_testdata_dir := $(spectest_threads_dir)/testdata
spec_version_threads := ff17701446d8e2086142423ef77ae947a025e26f

spectest_tail_call_dir := $(spectest_base_dir)/tail-call
spectest_tail_call_testdata_dir := $(spectest_tail_call_dir)/testdata
spec_version_tail_call := 88e97b0f742f4c3ee01fea683da130f344dd7b02

spectest_extended_const_dir := $(spectest_base_dir)/extended-const
spectest_extended_const_testdata_dir := $(spectest_extended_const_dir)/testdata
# These stay committed: they are a trimmed copy, inherited from wazero, of the proposal's data.wast, elem.wast
# and global.wast (WebAssembly/testsuite@0a394e3c proposals/extended-const), which exists nowhere upstream.
# The untrimmed files also assert 2.0's rule that a constant expression reads only imported globals, and
# wazy, with extended-const enabled, accepts a module-defined one.

spectest_exception_handling_dir := $(spectest_base_dir)/exception-handling
spectest_exception_handling_testdata_dir := $(spectest_exception_handling_dir)/testdata
spec_version_exception_handling := 13734f8fb871a5dab939070f893adbd90bffe28c

spectest_relaxed_simd_dir := $(spectest_base_dir)/relaxed-simd
spectest_relaxed_simd_testdata_dir := $(spectest_relaxed_simd_dir)/testdata
spec_version_relaxed_simd := c3f9359af2cd607cc46b0a3274f90ea52543a2f2

spectest_multi_memory_dir := $(spectest_base_dir)/multi-memory
spectest_multi_memory_testdata_dir := $(spectest_multi_memory_dir)/testdata
spec_version_multi_memory := cf8b5aa27257311b8eac80ae83f4ba22ee308064

spectest_memory64_dir := $(spectest_base_dir)/memory64
spectest_memory64_testdata_dir := $(spectest_memory64_dir)/testdata
spec_version_memory64 := 9003cd5e24e53b84cd9027ea3dd7ae57159a6db1
# The memory64 proposal has no test/core/memory64 subdirectory: its cases live
# alongside the rest of the core suite, either in a *64.wast counterpart of an
# existing file or mixed into one that covers both index types. This is the set
# that exercises an i64 index type, on a memory or on a table.
memory64_wast_files := \
	address64.wast align64.wast binary-leb128.wast call_indirect.wast \
	endianness64.wast float_memory64.wast imports.wast load64.wast \
	memory64.wast memory_copy.wast memory_fill.wast memory_grow64.wast \
	memory_init.wast memory_redundancy64.wast memory_trap64.wast \
	table.wast table_copy.wast table_copy_mixed.wast table_fill.wast \
	table_get.wast table_grow.wast table_init.wast table_set.wast \
	table_size.wast

spectest_typed_function_references_dir := $(spectest_base_dir)/typed-function-references
spectest_typed_function_references_testdata_dir := $(spectest_typed_function_references_dir)/testdata
spec_version_typed_function_references := 74d2ec81d15efd3c0f2fba46a023f376101d8e46
typed_function_references_wast_files := \
	br_on_non_null.wast br_on_null.wast br_table.wast call_ref.wast elem.wast \
	func.wast linking.wast local_init.wast ref.wast ref_as_non_null.wast \
	ref_func.wast ref_is_null.wast ref_null.wast return_call_indirect.wast \
	return_call_ref.wast return_call.wast select.wast table-sub.wast table.wast \
	type-equivalence.wast unreached-invalid.wast unreached-valid.wast

spectest_gc_dir := $(spectest_base_dir)/gc
spectest_gc_testdata_dir := $(spectest_gc_dir)/testdata
spec_version_gc := 756060f5816c7e2159f4817fbdee76cf52f9c923
# The whole of the GC proposal's core suite: everything under test/core plus
# everything under test/core/gc, which is why the download tries both paths.
# comments.wast is the one exclusion, and not for a GC reason: it pulls in .wat
# modules, which the harness (spectest.Run) only reads as .wasm.
# Recursive rather than immediate: an immediate assignment runs these two API calls on every make
# invocation, `make test` and `make lint` included, which is both slow and a third of the hourly
# unauthenticated budget spent before any target runs.
gc_wast_files = $(shell $(gh_api_curl) \
	'https://api.github.com/repos/WebAssembly/gc/contents/test/core?ref=$(spec_version_gc)' \
	| jq -r '.[] | select(.name|endswith(".wast")) | .name' | grep -v '^comments.wast$$') \
	$(shell $(gh_api_curl) \
	'https://api.github.com/repos/WebAssembly/gc/contents/test/core/gc?ref=$(spec_version_gc)' \
	| jq -r '.[] | select(.name|endswith(".wast")) | .name')

.PHONY: build.spectest
build.spectest:
	@$(MAKE) build.spectest.v1
	@$(MAKE) build.spectest.v2
	@$(MAKE) build.spectest.v3
	@$(MAKE) build.spectest.v3_interaction
	@$(MAKE) build.spectest.threads
	@$(MAKE) build.spectest.tail_call
	@$(MAKE) build.spectest.extended_const
	@$(MAKE) build.spectest.exception_handling
	@$(MAKE) build.spectest.typed_function_references
	@$(MAKE) build.spectest.relaxed_simd
	@$(MAKE) build.spectest.multi_memory
	@$(MAKE) build.spectest.memory64
	@$(MAKE) build.spectest.gc

.PHONY: build.spectest.v1
build.spectest.v1: $(wast2json) # Note: wabt by default uses >1.0 features, so wast2json flags might drift as they include more. See WebAssembly/wabt#1878
	@$(call clean_fetched,$(spectest_v1_testdata_dir))
	@cd $(spectest_v1_testdata_dir) \
		&& $(gh_api_curl) 'https://api.github.com/repos/WebAssembly/spec/contents/test/core?ref=$(spec_version_v1)' | jq -r '.[]| .download_url' | grep -E ".wast" | xargs -Iurl curl -sJL url -O
	@cd $(spectest_v1_testdata_dir) && for f in `find . -name '*.wast'`; do \
		perl -pi -e 's/\(assert_return_canonical_nan\s(\(invoke\s"f32.demote_f64"\s\((f[0-9]{2})\.const\s[a-z0-9.+:-]+\)\))\)/\(assert_return $$1 \(f32.const nan:canonical\)\)/g' $$f; \
		perl -pi -e 's/\(assert_return_arithmetic_nan\s(\(invoke\s"f32.demote_f64"\s\((f[0-9]{2})\.const\s[a-z0-9.+:-]+\)\))\)/\(assert_return $$1 \(f32.const nan:arithmetic\)\)/g' $$f; \
		perl -pi -e 's/\(assert_return_canonical_nan\s(\(invoke\s"f64\.promote_f32"\s\((f[0-9]{2})\.const\s[a-z0-9.+:-]+\)\))\)/\(assert_return $$1 \(f64.const nan:canonical\)\)/g' $$f; \
		perl -pi -e 's/\(assert_return_arithmetic_nan\s(\(invoke\s"f64\.promote_f32"\s\((f[0-9]{2})\.const\s[a-z0-9.+:-]+\)\))\)/\(assert_return $$1 \(f64.const nan:arithmetic\)\)/g' $$f; \
		perl -pi -e 's/\(assert_return_canonical_nan\s(\(invoke\s"[a-z._0-9]+"\s\((f[0-9]{2})\.const\s[a-z0-9.+:-]+\)\))\)/\(assert_return $$1 \($$2.const nan:canonical\)\)/g' $$f; \
		perl -pi -e 's/\(assert_return_arithmetic_nan\s(\(invoke\s"[a-z._0-9]+"\s\((f[0-9]{2})\.const\s[a-z0-9.+:-]+\)\))\)/\(assert_return $$1 \($$2.const nan:arithmetic\)\)/g' $$f; \
		perl -pi -e 's/\(assert_return_canonical_nan\s(\(invoke\s"[a-z._0-9]+"\s\((f[0-9]{2})\.const\s[a-z0-9.+:-]+\)\s\([a-z0-9.\s+-:]+\)\))\)/\(assert_return $$1 \($$2.const nan:canonical\)\)/g' $$f; \
		perl -pi -e 's/\(assert_return_arithmetic_nan\s(\(invoke\s"[a-z._0-9]+"\s\((f[0-9]{2})\.const\s[a-z0-9.+:-]+\)\s\([a-z0-9.\s+-:]+\)\))\)/\(assert_return $$1 \($$2.const nan:arithmetic\)\)/g' $$f; \
		perl -pi -e 's/\(assert_return_canonical_nan\s(\(invoke\s"[a-z._0-9]+"\s\((f[0-9]{2})\.const\s[a-z0-9.+:-]+\)\))\)/\(assert_return $$1 \($$2.const nan:canonical\)\)/g' $$f; \
		perl -pi -e 's/\(assert_return_arithmetic_nan\s(\(invoke\s"[a-z._0-9]+"\s\((f[0-9]{2})\.const\s[a-z0-9.+:-]+\)\))\)/\(assert_return $$1 \($$2.const nan:arithmetic\)\)/g' $$f; \
		$(wast2json) \
			--disable-saturating-float-to-int \
			--disable-sign-extension \
			--disable-simd \
			--disable-multi-value \
			--disable-bulk-memory \
			--disable-reference-types \
			--debug-names $$f; \
	done
	@rm -f $(spectest_v1_testdata_dir)/*.wast

.PHONY: build.spectest.v2
build.spectest.v2: $(wast2json) # Note: SIMD cases are placed in the "simd" subdirectory.
	@$(call clean_fetched,$(spectest_v2_testdata_dir))
	@cd $(spectest_v2_testdata_dir) \
		&& $(gh_api_curl) 'https://api.github.com/repos/WebAssembly/spec/contents/test/core?ref=$(spec_version_v2)' | jq -r '.[]| .download_url' | grep -E ".wast" | xargs -Iurl curl -sJL url -O
	@cd $(spectest_v2_testdata_dir) \
		&& $(gh_api_curl) 'https://api.github.com/repos/WebAssembly/spec/contents/test/core/simd?ref=$(spec_version_v2)' | jq -r '.[]| .download_url' | grep -E ".wast" | xargs -Iurl curl -sJL url -O
	@cd $(spectest_v2_testdata_dir) && for f in `find . -name '*.wast'`; do \
		$(wast2json) --debug-names --no-check $$f || true; \
	done # Ignore the error here as some tests (e.g. comments.wast right now) are not supported by wast2json yet.
	@rm -f $(spectest_v2_testdata_dir)/*.wast

.PHONY: build.spectest.v3
build.spectest.v3: $(wasm_tools) $(spectest_testsuite_dir)/binary.wast # Needs wasm-tools: the corpus spans every 3.0 proposal, including the GC type section and memory64's "module definition".
	@rm -rf $(spectest_v3_testdata_dir)
	@mkdir -p $(spectest_v3_testdata_dir)
	@cp $(spectest_testsuite_dir)/*.wast $(spectest_v3_testdata_dir)/
	@cd $(spectest_v3_testdata_dir) && for f in `find . -name '*.wast'`; do \
		$(wasm_tools) json-from-wast --wasm-dir . -o $$(basename $$f .wast).json $$f; \
	done
	@sh $(spectest_v3_dir)/assemble-text-modules.sh $(spectest_v3_testdata_dir)
	@rm -f $(spectest_v3_testdata_dir)/*.wast # The submodule is the source; these were a working copy.

.PHONY: build.spectest.v3_interaction
build.spectest.v3_interaction: $(wasm_tools) # Refreshes the vendored wasmtime cases, then rebuilds every case from its .wast.
	@cd $(spectest_v3_interaction_testdata_dir) \
		&& for f in $(wasmtime_interaction_wast_files); do \
			curl -sfJL "https://raw.githubusercontent.com/bytecodealliance/wasmtime/$(wasmtime_version)/tests/misc_testsuite/$$f" \
				-o "wasmtime-$$(echo $$f | tr '/' '-')"; \
		done
	@cd $(spectest_v3_interaction_testdata_dir) && rm -f *.json *.wasm && for f in `find . -name '*.wast'`; do \
		$(wasm_tools) json-from-wast --wasm-dir . -o $$(basename $$f .wast).json $$f; \
	done

# Note: We currently cannot build the "threads" subdirectory that spawns threads due to missing support in wast2json.
# https://github.com/WebAssembly/wabt/issues/2348#issuecomment-1878003959
#
# The cases come from two places, because neither has all of them:
#
#   - atomic.wast from the proposal repository. The testsuite mirror's copy is a strict subset of it,
#     71 lines short: no sub-word cmpxchg, and none of the wait/notify out-of-bounds or unaligned traps.
#   - exports.wast and memory.wast from the testsuite submodule. The proposal repository has files by
#     those names at the root of test/core, but they are the plain spec versions and mention `shared`
#     nowhere; the shared-memory cases exist only in the mirror's curated proposals/threads.
#
# imports.wast is deliberately not taken from either. The mirror's copy has the shared-memory import
# cases but the threads branch forked before reference-types, so it still asserts that a module with
# two tables is invalid -- which wazy correctly accepts. It also imports a "shared_memory" the spectest
# host module does not export.
.PHONY: build.spectest.threads
build.spectest.threads: $(wast2json) $(spectest_testsuite_dir)/binary.wast
	@$(call clean_fetched,$(spectest_threads_testdata_dir))
	@cd $(spectest_threads_testdata_dir) \
		&& $(gh_api_curl) 'https://api.github.com/repos/WebAssembly/threads/contents/test/core/threads?ref=$(spec_version_threads)' | jq -r '.[]| .download_url' | grep -E "/atomic.wast" | xargs -Iurl curl -sJL url -O
	@cp $(spectest_testsuite_dir)/proposals/threads/exports.wast $(spectest_testsuite_dir)/proposals/threads/memory.wast \
		$(spectest_threads_testdata_dir)/
	@cd $(spectest_threads_testdata_dir) && for f in `find . -name '*.wast'`; do \
		$(wast2json) --enable-threads --debug-names $$f; \
	done
	@rm -f $(spectest_threads_testdata_dir)/*.wast

.PHONY: build.spectest.extended_const
build.spectest.extended_const: $(wast2json)
	@cd $(spectest_extended_const_testdata_dir) && for f in `find . -name '*.wast'`; do \
		$(wast2json) --enable-extended-const --debug-names $$f; \
	done

.PHONY: build.spectest.tail_call
build.spectest.tail_call: $(wast2json)
	@$(call clean_fetched,$(spectest_tail_call_testdata_dir))
	@cd $(spectest_tail_call_testdata_dir) \
		&& $(gh_api_curl) 'https://api.github.com/repos/WebAssembly/testsuite/contents/proposals/tail-call?ref=$(spec_version_tail_call)' | jq -r '.[]| .download_url' | grep -E ".wast" | xargs -Iurl curl -sJL url -O
	@cd $(spectest_tail_call_testdata_dir) && for f in `find . -name '*.wast'`; do \
		$(wast2json) --enable-tail-call --debug-names $$f; \
	done
	@rm -f $(spectest_tail_call_testdata_dir)/*.wast

.PHONY: build.spectest.exception_handling
build.spectest.exception_handling: $(wasm_tools)
	@$(call clean_fetched,$(spectest_exception_handling_testdata_dir))
	@cd $(spectest_exception_handling_testdata_dir) \
		&& $(gh_api_curl) 'https://api.github.com/repos/WebAssembly/spec/contents/test/core/exceptions?ref=$(spec_version_exception_handling)' \
		| jq -r '.[]| .download_url' | grep -E ".wast" | xargs -Iurl curl -sJL url -O
	@cd $(spectest_exception_handling_testdata_dir) && for f in `find . -name '*.wast'`; do \
		$(wasm_tools) json-from-wast --wasm-dir . -o $$(basename $$f .wast).json $$f || true; \
	done
	@rm -f $(spectest_exception_handling_testdata_dir)/*.wast

.PHONY: build.spectest.typed_function_references
build.spectest.typed_function_references: $(wasm_tools)
	@$(call clean_fetched,$(spectest_typed_function_references_testdata_dir))
	@cd $(spectest_typed_function_references_testdata_dir) \
		&& for f in $(typed_function_references_wast_files); do \
			curl -sJL "https://raw.githubusercontent.com/WebAssembly/function-references/$(spec_version_typed_function_references)/test/core/$$f" -O; \
		done
	@cd $(spectest_typed_function_references_testdata_dir) && for f in `find . -name '*.wast'`; do \
		$(wasm_tools) json-from-wast --wasm-dir . -o $$(basename $$f .wast).json $$f || true; \
	done
	@rm -f $(spectest_typed_function_references_testdata_dir)/*.wast

.PHONY: build.spectest.relaxed_simd
build.spectest.relaxed_simd: $(wasm_tools) # Needs wasm-tools: wast2json drops the proposal's multi-result "either" expectations.
	@$(call clean_fetched,$(spectest_relaxed_simd_testdata_dir))
	@cd $(spectest_relaxed_simd_testdata_dir) \
		&& $(gh_api_curl) 'https://api.github.com/repos/WebAssembly/relaxed-simd/contents/test/core/relaxed-simd?ref=$(spec_version_relaxed_simd)' | jq -r '.[]| .download_url' | grep -E ".wast" | xargs -Iurl curl -sJL url -O
	@cd $(spectest_relaxed_simd_testdata_dir) && for f in `find . -name '*.wast'`; do \
		$(wasm_tools) json-from-wast --wasm-dir . -o $$(basename $$f .wast).json $$f; \
	done
	@rm -f $(spectest_relaxed_simd_testdata_dir)/*.wast

.PHONY: build.spectest.memory64
build.spectest.memory64: $(wasm_tools) # Needs wasm-tools: wast2json cannot emit the "module definition" command memory64.wast uses.
	@$(call clean_fetched,$(spectest_memory64_testdata_dir))
	@cd $(spectest_memory64_testdata_dir) \
		&& for f in $(memory64_wast_files); do \
			curl -sJL "https://raw.githubusercontent.com/WebAssembly/memory64/$(spec_version_memory64)/test/core/$$f" -O; \
		done
	@cd $(spectest_memory64_testdata_dir) && for f in `find . -name '*.wast'`; do \
		$(wasm_tools) json-from-wast --wasm-dir . -o $$(basename $$f .wast).json $$f; \
	done
	@rm -f $(spectest_memory64_testdata_dir)/*.wast

.PHONY: build.spectest.gc
build.spectest.gc: $(wasm_tools) # Needs wasm-tools: wast2json does not support the GC proposal's type section.
	@$(call clean_fetched,$(spectest_gc_testdata_dir))
	@cd $(spectest_gc_testdata_dir) \
		&& for f in $(gc_wast_files); do \
			curl -sfJL "https://raw.githubusercontent.com/WebAssembly/gc/$(spec_version_gc)/test/core/$$f" -o $$f \
			|| curl -sfJL "https://raw.githubusercontent.com/WebAssembly/gc/$(spec_version_gc)/test/core/gc/$$f" -o $$f; \
		done
	@cd $(spectest_gc_testdata_dir) && for f in `find . -name '*.wast'`; do \
		$(wasm_tools) json-from-wast --wasm-dir . -o $$(basename $$f .wast).json $$f; \
	done
	@rm -f $(spectest_gc_testdata_dir)/*.wast

.PHONY: build.spectest.multi_memory
build.spectest.multi_memory: $(wast2json)
	@$(call clean_fetched,$(spectest_multi_memory_testdata_dir))
	@cd $(spectest_multi_memory_testdata_dir) \
		&& $(gh_api_curl) 'https://api.github.com/repos/WebAssembly/multi-memory/contents/test/core/multi-memory?ref=$(spec_version_multi_memory)' | jq -r '.[]| .download_url' | grep -E ".wast" | xargs -Iurl curl -sJL url -O
	@cd $(spectest_multi_memory_testdata_dir) && for f in `find . -name '*.wast'`; do \
		$(wast2json) --enable-multi-memory --debug-names $$f; \
	done
	@rm -f $(spectest_multi_memory_testdata_dir)/*.wast

# Everything below is fetched from where it lives, at a pinned commit, rather than committed. Like the
# spectest suites, CI builds it once and hands it to every test job; without it the tests that need it
# skip, naming the target that fetches it.
.PHONY: build.testdata
build.testdata:
	@$(MAKE) build.spectest
	@$(MAKE) build.component_testdata
	@$(MAKE) build.wasi_testdata
	@$(MAKE) build.guest_testdata

# Component-model cases, generated into what the tests read with the pinned wasm-tools:
#
#   - WebAssembly/component-model's own suites. Its test/wasmtime holds cases that repository took in
#     from Wasmtime and wasm-tools.
#   - wasm-tools' and Wasmtime's own tests, where they reach what the component-model suite does not.
#     They are implementations' tests, not the specification's. Also wit-parser's UI suite.
#   - The Canonical ABI's reference implementation, definitions.py, which the abi package's oracle
#     generators import, and one module of resources.wast that wasip2 runs on its own.
#
# component-model is pinned twice. The conformance suites below are cf242a7's, generated with wasm-tools
# 1.253.0: that reproduces them byte for byte, while 1.262.0 encodes several differently. The
# max-value-size and fixed-length-list cases are later, and written against 1.262.0.
#
# One async suite, sync-streams, is a later commit's (#705): cf242a7's copy expects a result the spec's
# own reference implementation does not give, and #705 corrected it. It needs wasm-tools 1.262.0.
component_model_version := d095e2f60ddae1f9a1e9b1e2fb547ec1114da677
component_model_conformance_version := cf242a7a50d58aed650bc467212c5fb93433287b
component_model_sync_streams_version := 2f1326540f73f8d58237028f8331c048b3e44d76
wasm_tools_tests_version := 2e6b2d6db499501a91a0b88a848435345bef1f66
component_model_raw := https://raw.githubusercontent.com/WebAssembly/component-model/$(component_model_version)/test
wasm_tools_raw := https://raw.githubusercontent.com/bytecodealliance/wasm-tools/$(wasm_tools_tests_version)/tests/cli/component-model
wasmtime_raw := https://raw.githubusercontent.com/bytecodealliance/wasmtime/$(wasmtime_version)/tests/misc_testsuite/component-model
# <directory the tests read>=<.wast it is generated from>
component_wast_suites := \
	internal/component/binary/testdata/wast/component-model-max-value-size=$(component_model_raw)/validation/max-value-size.wast \
	internal/component/binary/testdata/wast/wasm-tools-max-value-size=$(wasm_tools_raw)/max-value-size.wast \
	internal/component/instance/testdata/wast/fixed_length_lists=$(wasmtime_raw)/fixed_length_lists.wast \
	internal/component/instance/testdata/wast/wasm-tools-fixed-length-lists=$(wasm_tools_raw)/fixed-length-lists.wast \
	internal/component/instance/testdata/wast/cm-validation-instantiation=$(component_model_raw)/validation/instantiation.wast \
	internal/component/instance/testdata/wast/cm-validation-resources=$(component_model_raw)/validation/resources.wast \
	internal/component/instance/testdata/wast/wasm-tools-instantiate=$(wasm_tools_raw)/instantiate.wast \
	internal/component/instance/testdata/wast/wasm-tools-virtualize=$(wasm_tools_raw)/virtualize.wast \
	internal/component/instance/testdata/wast/wasmtime-nested=$(wasmtime_raw)/nested.wast \
	internal/component/instance/testdata/wast/wasmtime-restrictions=$(wasmtime_raw)/restrictions.wast
wit_parser_ui_dir := component/wit/testdata/wit-parser/ui
component_model_dir := build/component-model
component_wast_dir := internal/component/instance/testdata/wast
component_async_dir := internal/component/instance/testdata/wast-async
# <directory under $(component_wast_dir)>=<.wast under component-model's test/>
component_conformance_suites := concat=values/concat strings=values/strings \
	multiple-resources=resources/multiple-resources fused=wasmtime/fused \
	resources=wasmtime/resources simple=wasmtime/simple types=wasmtime/types

.PHONY: build.component_testdata
build.component_testdata: $(wasm_tools) $(wasm_tools_next)
	@for s in $(component_wast_suites); do \
		dir=$${s%%=*} url=$${s#*=} f=$${s##*/}; \
		rm -rf $$dir && mkdir -p $$dir \
		&& curl -sfSL --retry 5 --retry-all-errors $$url -o $$dir/$$f \
		&& (cd $$dir && $(wasm_tools_next) json-from-wast --wasm-dir . -o $$(basename $$f .wast).json $$f) || exit 1; \
	done
	@rm -rf $(wit_parser_ui_dir) && mkdir -p $(wit_parser_ui_dir)
	@curl -sfSL --retry 5 --retry-all-errors \
		https://codeload.github.com/bytecodealliance/wasm-tools/tar.gz/$(wasm_tools_tests_version) \
		| tar -xzf - -C $(wit_parser_ui_dir) --strip-components 5 \
			wasm-tools-$(wasm_tools_tests_version)/crates/wit-parser/tests/ui
	@PATH="$(dir $(wasm_tools_next)):$$PATH" sh component/wit/testdata/gen-oracle.sh
	@rm -rf $(component_model_dir) && mkdir -p $(component_model_dir)
	@curl -fsSL --retry 5 --retry-all-errors \
		https://codeload.github.com/WebAssembly/component-model/tar.gz/$(component_model_conformance_version) \
		| tar -xzf - -C $(component_model_dir) --strip-components 1 \
			component-model-$(component_model_conformance_version)/test \
			component-model-$(component_model_conformance_version)/design/mvp/canonical-abi/definitions.py
	@for s in $(component_conformance_suites); do \
		rm -rf $(component_wast_dir)/$${s%%=*} && mkdir -p $(component_wast_dir)/$${s%%=*} \
		&& cp $(component_model_dir)/test/$${s#*=}.wast $(component_wast_dir)/$${s%%=*}/ || exit 1; \
	done
	@for f in $(component_model_dir)/test/async/*.wast; do \
		s=$$(basename $$f .wast); [ $$s = sync-streams ] && continue; \
		rm -rf $(component_async_dir)/$$s && mkdir -p $(component_async_dir)/$$s && cp $$f $(component_async_dir)/$$s/; \
	done
	@for d in $(foreach s,$(component_conformance_suites),$(component_wast_dir)/$(firstword $(subst =, ,$(s)))) \
		$(component_async_dir)/*; do \
		[ $$(basename $$d) = sync-streams ] && continue; \
		(cd $$d && $(wasm_tools) json-from-wast --wasm-dir . -o $$(basename $$d).json $$(basename $$d).wast) || exit 1; \
	done
	@rm -rf $(component_async_dir)/sync-streams && mkdir -p $(component_async_dir)/sync-streams \
		&& curl -sfSL --retry 5 --retry-all-errors -o $(component_async_dir)/sync-streams/sync-streams.wast \
			https://raw.githubusercontent.com/WebAssembly/component-model/$(component_model_sync_streams_version)/test/async/sync-streams.wast \
		&& (cd $(component_async_dir)/sync-streams && $(wasm_tools_next) json-from-wast --wasm-dir . -o sync-streams.json sync-streams.wast)
	@$(call clean_fetched,imports/wasip2/testdata/component-model)
	@cp $(component_wast_dir)/resources/resources.20.wasm imports/wasip2/testdata/component-model/
	@cp $(component_model_dir)/design/mvp/canonical-abi/definitions.py internal/component/abi/testdata/

# WASI's own interface definitions, from WebAssembly/WASI at the commits its release tags name. wasip2
# checks every host import it registers against 0.2.12's; the wit parser's tests read two 0.2.8 files.
wasi_version_0_2_12 := 281ba75fafcd50961ef55f9e52747afcc9b71ede
wasi_version_0_2_8 := b26023f7d540bc7f0faae24052ca887421541acf
wasi_dir := build/wasi
wasip2_wit_dir := imports/wasip2/testdata/wasi-wit
wasip2_wit_packages := cli clocks filesystem http io random sockets

.PHONY: build.wasi_testdata
build.wasi_testdata:
	@rm -rf $(wasi_dir) && mkdir -p $(wasi_dir)
	@curl -fsSL --retry 5 --retry-all-errors \
		https://codeload.github.com/WebAssembly/WASI/tar.gz/$(wasi_version_0_2_12) \
		| tar -xzf - -C $(wasi_dir) --strip-components 1 WASI-$(wasi_version_0_2_12)/proposals
	@$(call clean_fetched,$(wasip2_wit_dir))
	@for p in $(wasip2_wit_packages); do \
		mkdir -p $(wasip2_wit_dir)/$$p && cp $(wasi_dir)/proposals/$$p/wit/*.wit $(wasip2_wit_dir)/$$p/ || exit 1; \
	done
	@for f in io/streams random/random; do \
		curl -fsSL --retry 5 --retry-all-errors \
			https://raw.githubusercontent.com/WebAssembly/WASI/$(wasi_version_0_2_8)/wasip2/$$f.wit \
			-o internal/component/wit/testdata/$$(basename $$f).wit || exit 1; \
	done

# Guests other projects test their own hosts with, fetched from those projects: dapr's wasi-http
# fixture, which wasi_http has to run the way dapr does, and http-wasm's TCK guest and trapping guest
# for http_handler. http-wasm's example guests stay committed beside the latter: they back testable
# examples, and an example cannot skip when its guest is missing.
dapr_contrib_version := 3b9591ed81daddb02e2482289caafe8f44c304c3
http_wasm_version := d9e07495db6c4192ce0995de036e2ad450616883
# <file the tests read>=<URL it is fetched from>
guest_testdata := \
	imports/wasi_http/testdata/http/main.wasm=https://raw.githubusercontent.com/dapr/components-contrib/$(dapr_contrib_version)/bindings/wasm/testdata/http/main.wasm \
	imports/http_handler/nethttp/internal/tck/testdata/tck.wasm=https://raw.githubusercontent.com/http-wasm/http-wasm-host-go/$(http_wasm_version)/tck/tck.wasm \
	imports/http_handler/nethttp/testdata/panic_on_handle_request.wasm=https://raw.githubusercontent.com/http-wasm/http-wasm-host-go/$(http_wasm_version)/internal/test/testdata/error/panic_on_handle_request.wasm

.PHONY: build.guest_testdata
build.guest_testdata:
	@for g in $(guest_testdata); do \
		curl -fsSL --retry 5 --retry-all-errors $${g#*=} -o $${g%%=*} || exit 1; \
	done

# The wasi:otel guests' WIT is upstream's, apart from the world each guest declares: the proposal's
# current WIT for otelguest, and the opentelemetry-wasi SDK's, which still has the old span-context
# name, for outerguest. Only rebuilding a guest needs it; the tests embed the built .wasm.
wasi_otel_version := e5262a84044aff064c9599eebc05013bb3432795
opentelemetry_wasi_version := 2962a286aa66188314bcafd79c800c4e39ce41e1
wasi_otel_wit_deps := deps/wasi-clocks-0.2.0/package.wit deps/wasi-io-0.2.0/package.wit

.PHONY: build.wasi_otel_wit
build.wasi_otel_wit:
	@for f in types.wit tracing.wit metrics.wit logs.wit $(wasi_otel_wit_deps); do \
		curl -fsSL --retry 5 --retry-all-errors --create-dirs \
			https://raw.githubusercontent.com/WebAssembly/wasi-otel/$(wasi_otel_version)/wit/$$f \
			-o imports/wasi_otel/testdata/otelguest/wit/$$f || exit 1; \
	done
	@for f in types.wit tracing.wit $(wasi_otel_wit_deps); do \
		curl -fsSL --retry 5 --retry-all-errors --create-dirs \
			https://raw.githubusercontent.com/bytecodealliance/opentelemetry-wasi/$(opentelemetry_wasi_version)/wit/$$f \
			-o imports/wasi_otel/testdata/outerguest/wit/$$f || exit 1; \
	done

# Builds one build image and tests it: its samples are built in the image,
# validated, and run under wazy from this checkout. See build-images/README.md.
#   make build-image-test IMAGE_LANG=rust
.PHONY: build-image-test
build-image-test:
	@test -n "$(IMAGE_LANG)" || (echo "usage: make build-image-test IMAGE_LANG=<$$(ls build-images/*/Dockerfile | cut -d/ -f2 | tr '\n' ' ')>"; exit 2)
	build-images/test.sh $(IMAGE_LANG)

.PHONY: test
test:
	@go test $(go_test_options) ./...
	@cd internal/version/testdata && go test $(go_test_options) ./...
	@cd internal/integration_test/fuzz/wazylib && CGO_ENABLED=0 WASM_BINARY_PATH=testdata/test.wasm go test ./...

.PHONY: test.arm64
test.arm64: ## Run the suite for the arm64 compiler backend under qemu-user
	# -one-insn-per-tb works around a qemu-user (<=8.2.2) multi-insn-TB self-modifying-code
	# bug that flaky-SIGSEGVs (~30%) on wazy's JIT'd code. Needs qemu-aarch64-static. See
	# CONTRIBUTING.md. Slower emulation, so a longer timeout than the host `test` target.
	@GOARCH=arm64 CGO_ENABLED=0 go test -timeout 90m -exec 'qemu-aarch64-static -one-insn-per-tb' ./...

.PHONY: test.interp
test.interp: ## Run the suite against the interpreter engine (riscv64 cross-run, no compiler)
	# A non-amd64/arm64 GOARCH auto-selects the interpreter; qemu-riscv64 runs it (no JIT, no
	# flag needed). Compiler-codegen tests self-skip via platform.CompilerSupported(). Needs
	# qemu-riscv64-static.
	@GOARCH=riscv64 CGO_ENABLED=0 go test -timeout 60m -exec qemu-riscv64-static ./...

.PHONY: coverage
# replace spaces with commas
coverpkg = $(shell echo $(main_packages) | tr ' ' ',')
coverage: ## Generate test coverage
	@go test -coverprofile=coverage.txt -covermode=atomic --coverpkg=$(coverpkg) $(main_packages)
	@go tool cover -func coverage.txt

golangci_lint_path := $(shell go env GOPATH)/bin/golangci-lint

$(golangci_lint_path):
	@go install $(golangci_lint)

golangci_lint_goarch ?= $(shell go env GOARCH)

.PHONY: lint
lint: $(golangci_lint_path)
	@GOARCH=$(golangci_lint_goarch) CGO_ENABLED=0 $(golangci_lint_path) run --timeout 5m -E testableexamples

.PHONY: format
format:
	@go run $(gofumpt) -l -w .
	@go run $(gosimports) -local github.com/samyfodil/ -w $(shell find . -name '*.go' -type f)
	@go run $(asmfmt) -w $(shell find . -name '*.s' -type f)

.PHONY: check  # Pre-flight check for pull requests
check:
# The following checks help ensure our platform-specific code used for system
# calls safely falls back on a platform unsupported by the compiler engine.
# This makes sure the intepreter can be used. Most often the package that can
# drift here is "platform" or "sysfs":
#
# Ensure we build on plan9. See #1578
	@GOARCH=amd64 GOOS=plan9 go build ./...
# Ensure we build on gojs. See #1526.
	@GOARCH=wasm GOOS=js go build ./...
# Ensure we build on wasip1. See #1526.
	@GOARCH=wasm GOOS=wasip1 go build ./...
# Ensure we build on aix. See #1723
	@GOARCH=ppc64 GOOS=aix go build ./...
# Ensure we build on linux s390x. See #2412
	@GOARCH=s390x GOOS=linux go build ./...
# Ensure we build on linux ppc64le. See #2412
	@GOARCH=ppc64le GOOS=linux go build ./...
# Ensure we build on linux arm for Dapr:
#	gh release view -R dapr/dapr --json assets --jq 'first(.assets[] | select(.name = "daprd_linux_arm.tar.gz") | {url, downloadCount})'
	@GOARCH=arm GOOS=linux go build ./...
# Ensure we build on linux 386 for Trivy:
#	gh release view -R aquasecurity/trivy --json assets --jq 'first(.assets[] | select(.name| test("Linux-32bit.*tar.gz")) | {url, downloadCount})'
	@GOARCH=386 GOOS=linux go build ./...
# Ensure we build on FreeBSD amd64 for Trivy:
#	gh release view -R aquasecurity/trivy --json assets --jq 'first(.assets[] | select(.name| test("FreeBSD-64bit.*tar.gz")) | {url, downloadCount})'
	@GOARCH=amd64 GOOS=freebsd go build ./...
	@$(MAKE) lint golangci_lint_goarch=arm64
	@$(MAKE) lint golangci_lint_goarch=amd64
	@$(MAKE) format
	@go mod tidy
	@if [ ! -z "`git status -s`" ]; then \
		echo "The following differences will fail CI until committed:"; \
		git diff --exit-code; \
	fi

.PHONY: clean
clean: ## Ensure a clean build
	@rm -rf dist build coverage.txt
	@go clean -testcache

fuzz_default_flags := --no-trace-compares --sanitizer=none -- -rss_limit_mb=8192

fuzz_timeout_seconds ?= 10
.PHONY: fuzz
fuzz:
	@cd internal/integration_test/fuzz && cargo test
	@cd internal/integration_test/fuzz && cargo fuzz run logging_no_diff $(fuzz_default_flags) -max_total_time=$(fuzz_timeout_seconds)
	@cd internal/integration_test/fuzz && cargo fuzz run no_diff $(fuzz_default_flags) -max_total_time=$(fuzz_timeout_seconds)
	@cd internal/integration_test/fuzz && cargo fuzz run memory_no_diff $(fuzz_default_flags) -max_total_time=$(fuzz_timeout_seconds)
	@cd internal/integration_test/fuzz && cargo fuzz run validation $(fuzz_default_flags) -max_total_time=$(fuzz_timeout_seconds)

libsodium:
	mkdir -p ./internal/integration_test/libsodium/testdata && cd ./internal/integration_test/libsodium/testdata && \
		curl -fsSL --retry 5 --retry-all-errors $(if $(GITHUB_TOKEN),-H "Authorization: Bearer $(GITHUB_TOKEN)") \
		"https://api.github.com/repos/jedisct1/webassembly-benchmarks/contents/2022-12/wasm?ref=7e86d68e99e60130899fbe3b3ab6e9dce9187a7c" \
		| jq -r '.[].download_url' | xargs -n 1 curl -fsSL --retry 5 --retry-all-errors -O

#### CLI release related ####

VERSION ?= dev
non_windows_platforms := darwin_amd64 darwin_arm64 linux_amd64 linux_arm64
non_windows_archives  := $(non_windows_platforms:%=dist/wazy_$(VERSION)_%.tar.gz)
windows_platforms     := windows_amd64 # TODO: add arm64 windows once we start testing on it.
windows_archives      := $(windows_platforms:%=dist/wazy_$(VERSION)_%.zip)
checksum_txt          := dist/wazy_$(VERSION)_checksums.txt

# define macros for multi-platform builds. these parse the filename being built
go-arch = $(if $(findstring amd64,$1),amd64,arm64)
go-os   = $(if $(findstring .exe,$1),windows,$(if $(findstring linux,$1),linux,darwin))

build/wazy_%/wazy:
	$(call go-build,$@,$<)

build/wazy_%/wazy.exe:
	$(call go-build,$@,$<)

dist/wazy_$(VERSION)_%.tar.gz: build/wazy_%/wazy
	@echo tar.gz "tarring $@"
	@mkdir -p $(@D)
# On Windows, we pass the special flag `--mode='+rx' to ensure that we set the executable flag.
# This is only supported by GNU Tar, so we set it conditionally.
	@tar -C $(<D) -cpzf $@ $(if $(findstring Windows_NT,$(OS)),--mode='+rx',) $(<F)
	@echo tar.gz "ok"

define go-build
	@echo "building $1"
	@# $(go:go=) removes the trailing 'go', so we can insert cross-build variables
	@$(go:go=) CGO_ENABLED=0 GOOS=$(call go-os,$1) GOARCH=$(call go-arch,$1) go build \
		-ldflags "-s -w -X github.com/samyfodil/wazy/internal/version.version=$(VERSION)" \
		-o $1 $2 ./cmd/wazy
	@echo build "ok"
endef

dist/wazy_$(VERSION)_%.zip: build/wazy_%/wazy.exe
	@echo zip "zipping $@"
	@mkdir -p $(@D)
	@zip -qj $@ $<
	@echo zip "ok"

# Darwin doesn't have sha256sum. See https://github.com/actions/virtual-environments/issues/90
sha256sum := $(if $(findstring darwin,$(shell go env GOOS)),shasum -a 256,sha256sum)
$(checksum_txt): $(non_windows_archives) $(windows_archives)
	@cd $(@D); touch $(@F); $(sha256sum) * > $(@F)

dist: $(non_windows_archives) $(windows_archives) $(checksum_txt)
