package instance

import (
	"bytes"
	"context"
	_ "embed"
	"io"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/abi"
)

// This file implements a DIFFERENTIAL CONFORMANCE harness: a battery of
// real rustc wasm32-wasip2 wasi:cli/command components (testdata/
// conformance/*.component.wasm), each with a golden stdout
// (testdata/conformance/*.stdout.golden) captured ONCE from wasmtime (the
// reference implementation) via:
//
//	wasmtime run <component> [args...] [--env K=V ...] [--dir <tmp>::/]
//
// TestConformance runs every fixture on wazy through the exact same
// WithWASI(Args/Env/FS) inputs used to produce its golden, and asserts
// wazy's stdout is byte-for-byte IDENTICAL to wasmtime's. wasmtime itself
// is NOT required at test time -- only the committed golden files are
// read (via go:embed), so this test runs anywhere Go runs.
//
// Unlike real_hello_test.go/real_args_test.go/etc (each proving ONE
// specific WASI call path in isolation, with hand-picked assertions),
// this harness is deliberately generic and wasmtime-referential: the
// fixtures were chosen to span distinct ABI/WASI feature axes (numeric
// formatting, unicode strings, args, env, collections lowered as
// list<T>/tuple<T>, recursion/variant/option/result, file read, file
// write+read-back, wasi:cli/exit, and large/streamed output), and the
// pass/fail signal is "does wazy's output match the reference
// implementation's", not any hand-authored expectation.
//
// # Fixture manifest
//
// Each entry below documents, in one place, exactly what Rust source
// produced the fixture and what wasmtime invocation produced its golden --
// the SAME Args/Env/FS must be threaded through WithWASI so wazy sees
// identical inputs.
var conformanceFixtures = []conformanceFixture{
	{
		name: "f01_arith",
		desc: "loops summing i64, println! width/precision floats, hex/HEX/bin/oct + alternate-form formatting",
		wasm: f01ArithWasm, golden: f01ArithGolden,
	},
	{
		name: "f02_strings",
		desc: "split/join/replace/.chars().rev(), to_uppercase, unicode (héllo wörld, CJK, emoji) -- exercises utf8 lowering/lifting",
		wasm: f02StringsWasm, golden: f02StringsGolden,
	},
	{
		name: "f03_args",
		desc: "std::env::args(): argc, per-arg echo, integer sum, join -- exercises WASIConfig.Args -> wasi:cli/environment.get-arguments",
		wasm: f03ArgsWasm, golden: f03ArgsGolden,
		args: []string{"10", "20", "hello", "5"},
	},
	{
		name: "f04_env",
		desc: "std::env::var/vars(): specific + prefix-filtered lookups -- exercises WASIConfig.Env -> wasi:cli/environment.get-environment",
		wasm: f04EnvWasm, golden: f04EnvGolden,
		env: []string{"WAZY_NAME=wazy", "WAZY_COUNT=42", "PATH_LIKE=/usr/bin"},
	},
	{
		name: "f05_collections",
		desc: "Vec sort/dedup/retain/map, HashMap insert + key-sorted iteration -- exercises list<T> lowering at nontrivial size/shape",
		wasm: f05CollectionsWasm, golden: f05CollectionsGolden,
	},
	{
		name: "f06_recursion",
		desc: "recursive fib/factorial, enum match (area-by-shape), Result<_,String>, Option -- exercises deep call stacks + variant/option/result value flow",
		wasm: f06RecursionWasm, golden: f06RecursionGolden,
	},
	{
		name: "f07_fileread",
		desc: "std::fs::read_to_string + to_uppercase -- exercises wasi:filesystem/types read path (preopens, descriptor, input-stream) via WASIConfig.FS",
		wasm: f07FilereadWasm, golden: f07FilereadGolden,
		fs: map[string][]byte{"/data.txt": f07FilereadInput},
	},
	{
		name: "f08_filewrite",
		desc: "std::fs::write then read_to_string -- exercises wasi:filesystem/types write path (open-at create/truncate, write-via-stream) via WASIConfig.FS",
		wasm: f08FilewriteWasm, golden: f08FilewriteGolden,
		fs: map[string][]byte{},
	},
	{
		name: "f09_exit",
		desc: "println! then std::process::exit(7) -- exercises wasi:cli/exit.exit; wasmtime maps any nonzero guest exit to reference process exit code 1 (wasi:cli/exit's status is result<_,_>, a bare Ok/Err signal with no room for an arbitrary integer code, so 7 itself is not observable through this interface on either implementation)",
		wasm: f09ExitWasm, golden: f09ExitGolden,
		wantCallErr: true,
	},
	{
		name: "f10_bigout",
		desc: "1000 numbered println! lines -- exercises repeated/streamed output-stream.write calls at volume",
		wasm: f10BigoutWasm, golden: f10BigoutGolden,
	},

	// --- harder batch: stresses the Canonical ABI + WASI far more than the
	// first ten (float/int formatting edge cases, serde_json record/list
	// marshalling, 10k/1k-element collections, deep non-tail recursion,
	// unicode casing/boundary edge cases, multi-handle filesystem I/O, a
	// guest trap via panic!, and long iterator/closure chains). ---

	{
		name: "f11_floats",
		desc: "NaN/inf/-0.0/subnormals/MAX/MIN/EPSILON, {:e}/{:.17}/{:.0} formatting -- exercises float lift/lower + Rust's exact Display/LowerExp formatting",
		wasm: f11FloatsWasm, golden: f11FloatsGolden,
	},
	{
		name: "f12_ints",
		desc: "i8/i16/i32/i64/i128/u128 MIN/MAX, wrapping/checked/saturating/overflowing ops, hex/oct/bin of negatives, float<->int as-casts (incl. NaN/out-of-range saturating casts) -- exercises integer width handling",
		wasm: f12IntsWasm, golden: f12IntsGolden,
	},
	{
		name: "f13_json",
		desc: "serde_json::Value parse/mutate/re-serialize + typed struct roundtrip via serde derive, unicode string field -- hammers string/list/record marshalling and allocation through a real crate dependency",
		wasm: f13JsonWasm, golden: f13JsonGolden,
	},
	{
		name: "f14_largedata",
		desc: "10k-element Vec (xorshift-filled) sort/binary-search/dedup/sum, 1000-entry BTreeMap insert+range+iterate, 50KB string build -- stresses allocation + large linear-memory ops",
		wasm: f14LargedataWasm, golden: f14LargedataGolden,
	},
	{
		name: "f15_deeprec",
		desc: "ackermann(3,3), 5000-deep non-tail recursion, 1000-deep boxed-tree build/walk, mutual recursion -- exercises guest call-stack depth",
		wasm: f15DeeprecWasm, golden: f15DeeprecGolden,
	},
	{
		name: "f16_unicode",
		desc: "multi-byte upper/lowercasing (ß->SS, Turkish İ/ı), char boundaries, combining marks, ZWJ emoji sequences, char_indices, unicode split/trim/classification -- exercises utf8 edge cases beyond f02_strings",
		wasm: f16UnicodeWasm, golden: f16UnicodeGolden,
	},
	{
		name: "f17_multifs",
		desc: "3 preopened files read + concatenated sorted by name, 2 files written and read back, interleaved multi-handle reads, overwrite, stat, missing-file error -- stresses the fs descriptor/stream path with several live handles at once",
		wasm: f17MultifsWasm, golden: f17MultifsGolden,
		fs: map[string][]byte{
			"/alpha.txt": f17MultifsInputAlpha,
			"/beta.txt":  f17MultifsInputBeta,
			"/gamma.txt": f17MultifsInputGamma,
		},
	},
	{
		name: "f18_panic",
		desc: "prints several lines then panic!(): wasmtime traps on the guest's `unreachable` (panic -> abort -> unreachable) after printing stdout up through the panic; stdout must still match up to that point and wazy must also report a call error (compare STDOUT only, per wasmtime's own stderr/backtrace being non-deterministic and out of scope)",
		wasm: f18PanicWasm, golden: f18PanicGolden,
		wantCallErr: true,
	},
	{
		name: "f19_iterchains",
		desc: "filter/map/flat_map/collect, zip/enumerate/fold, windows/chunks/scan, HashMap group-by (key-sorted for determinism), returned closures, Option/Result combinators, partition, take_while/skip_while, sort_by_key -- exercises complex iterator/closure chains with formatting",
		wasm: f19IterchainsWasm, golden: f19IterchainsGolden,
	},

	// --- batch 3: two axes nothing above exercises -- (1) stdin, the last
	// untested host->guest streaming data-flow direction (f20-f23: wired
	// wasi:cli/stdin.get-stdin -> wasi_fs.go's fsStreamNode/input-stream.
	// {read,blocking-read} machinery, backed by WASIConfig.Stdin instead of
	// a file -- see wasi.go's getStdin doc), and (2) real third-party
	// crates.io dependencies beyond serde_json (f13_json already covered
	// serde; f24-f28 add regex, sha2+hex, base64, csv+serde, itertools). ---

	{
		name: "f20_cat",
		desc: "std::io::stdin().read_to_end() then print verbatim + byte count -- simplest possible get-stdin -> input-stream.{read,blocking-read} -> EOF exercise, raw bytes (not read_to_string) including multi-byte UTF-8 and no trailing newline",
		wasm: f20CatWasm, golden: f20CatGolden,
		stdin: f20CatStdin,
	},
	{
		name: "f21_wc",
		desc: "std::io::stdin().read_to_string() then print line/word/byte counts (mimics `wc`) -- exercises the utf8 read_to_string path over stdin instead of f20_cat's raw read_to_end",
		wasm: f21WcWasm, golden: f21WcGolden,
		stdin: f21WcStdin,
	},
	{
		name: "f22_grep",
		desc: "read stdin, print 1-indexed lines containing a substring pattern taken from argv (case-sensitive str::contains) -- exercises stdin input-stream together with WASIConfig.Args in the same run",
		wasm: f22GrepWasm, golden: f22GrepGolden,
		stdin: f22GrepStdin,
		args:  []string{"error"},
	},
	{
		name: "f23_upperstdin",
		desc: "read stdin (accented + CJK + emoji), print uppercased + char count -- Unicode case conversion fed entirely through the stdin input-stream path (vs f02_strings/f16_unicode's string literals), so the guest must assemble UTF-8 across the read-to-string boundary correctly",
		wasm: f23UpperstdinWasm, golden: f23UpperstdinGolden,
		stdin: f23UpperstdinStdin,
	},
	{
		name: "f24_regex",
		desc: "real `regex` crate: capture groups (email/phone), find_iter with byte offsets, replace_all (twice, chained), split on a pattern, is_match -- a real third-party crate with its own NFA/DFA engine and heap-heavy internals",
		wasm: f24RegexWasm, golden: f24RegexGolden,
	},
	{
		name: "f25_sha2",
		desc: "real `sha2` + `hex` crates: SHA-256/SHA-512 of empty/ascii/unicode/10000-byte inputs, plus an incremental multi-update hash matching a single-shot hash of the concatenation -- deterministic cryptographic hashing through a real crate",
		wasm: f25Sha2Wasm, golden: f25Sha2Golden,
	},
	{
		name: "f26_base64",
		desc: "real `base64` crate: standard/URL-safe/no-pad encode, decode round-trip of empty/ascii/unicode/all-256-byte-values/binary-with-nulls data -- exercises a real crate over raw non-UTF8 byte data end to end",
		wasm: f26Base64Wasm, golden: f26Base64Golden,
	},
	{
		name: "f27_csv",
		desc: "real `csv` + `serde` derive crates: deserialize CSV text into typed structs, filter/transform/sort, re-serialize via csv::Writer -- exercises a real crate's record<->struct marshalling (including float formatting) end to end, in-memory only",
		wasm: f27CsvWasm, golden: f27CsvGolden,
	},
	{
		name: "f28_itertools",
		desc: "real `itertools` crate: chunks, tuple_windows, cartesian_product, chunk_by, kmerge, zip_longest, dedup, unique, minmax -- heavy iterator-combinator usage over deterministic in-memory data",
		wasm: f28ItertoolsWasm, golden: f28ItertoolsGolden,
	},

	// --- batch 4: the WASI filesystem surface no fixture above touches --
	// directory listing/scoping and random-access reads/writes -- each
	// chosen to hit a wasi:filesystem/types descriptor method this package
	// didn't implement yet (see wasi_fs.go's "batch 4" doc addendum for the
	// discovery + implementation notes). f07/f08/f17 above only ever
	// open-at + read/write-via-stream a single flat file at a time; these
	// seven are the first fixtures whose guest calls
	// [method]descriptor.read-directory, seeks within an open file, or
	// removes a path. ---

	{
		name: "f29_readdir",
		desc: "std::fs::read_dir(\"/\") over 2 files + 1 subdirectory, name-sorted, plus a count -- exercises [method]descriptor.read-directory -> directory-entry-stream resource -> [method]directory-entry-stream.read-directory-entry",
		wasm: f29ReaddirWasm, golden: f29ReaddirGolden,
		fs: map[string][]byte{"/a.txt": []byte("A"), "/b.txt": []byte("BB"), "/sub/inner.txt": []byte("inner")},
	},
	{
		name: "f30_filetypes",
		desc: "std::fs::read_dir(\"/\") then entry.file_type()/metadata().len() per entry, distinguishing files from a synthetic directory -- exercises DirEntryType::type together with read-directory's file-vs-dir shape",
		wasm: f30FiletypesWasm, golden: f30FiletypesGolden,
		fs: map[string][]byte{"/a.txt": []byte("A"), "/b.txt": []byte("BB"), "/sub/inner.txt": []byte("inner")},
	},
	{
		name: "f31_seek",
		desc: "open a file, Seek::Start/Current/End then read a slice at each position -- exercises [method]descriptor.read-via-stream(offset) called repeatedly against one open descriptor as std::io::Seek's backing implementation",
		wasm: f31SeekWasm, golden: f31SeekGolden,
		fs: map[string][]byte{"/data.txt": []byte("0123456789ABCDEFGHIJ world hello")},
	},
	{
		name: "f32_nested",
		desc: "3 files under a nested path (/a/b.txt, /a/c.txt, /d.txt), listing \"/\" and \"/a\" separately then reading a nested file -- exercises path resolution + directory scoping over the flat host-FS map (no directory ever explicitly present as its own map key)",
		wasm: f32NestedWasm, golden: f32NestedGolden,
		fs: map[string][]byte{"/a/b.txt": []byte("b content"), "/a/c.txt": []byte("c content"), "/d.txt": []byte("d content")},
	},
	{
		name: "f33_createlist",
		desc: "std::fs::write creates a new file, then read_dir(\"/\") shows it alongside a read-back of its content -- exercises write-then-list consistency: read-directory must observe an open-at(create) commit from earlier in the same run",
		wasm: f33CreatelistWasm, golden: f33CreatelistGolden,
		fs: map[string][]byte{},
	},
	{
		name: "f34_append",
		desc: "write, stat len, OpenOptions::append + write_all, stat len again, read back, then overwrite (truncate) + stat len a third time -- exercises append-via-stream's write-cursor seeded at the file's current end, together with stat's size field staying live across writes",
		wasm: f34AppendWasm, golden: f34AppendGolden,
		fs: map[string][]byte{},
	},
	{
		name: "f35_remove",
		desc: "list \"/\", std::fs::remove_file, list \"/\" again, then attempt to read the removed path and print io::ErrorKind -- exercises [method]descriptor.unlink-file-at",
		wasm: f35RemoveWasm, golden: f35RemoveGolden,
		fs: map[string][]byte{"/gone.txt": []byte("x")},
	},
}

type conformanceFixture struct {
	name string
	desc string

	wasm   []byte
	golden string

	args []string
	env  []string
	fs   map[string][]byte

	// stdin, when non-nil, is fed to the guest via WASIConfig.Stdin (as a
	// bytes.Reader) -- the exact bytes wasmtime's golden was captured
	// against (see the fixture manifest doc's batch-3 note and wasi.go's
	// getStdin doc). A nil stdin (every fixture before f20_cat) leaves
	// WASIConfig.Stdin nil, matching wasmtime's default of an empty stdin
	// for fixtures that never read it.
	stdin []byte

	// wantCallErr is true for fixtures whose guest terminates via
	// wasi:cli/exit with a nonzero status (f09_exit): wazy has no host
	// process for a successful OR failing exit() to terminate (see
	// wasi.go's exit doc), so run() surfaces as a Go error either way, but
	// stdout written before the exit call is still fully captured and must
	// still match the golden exactly.
	wantCallErr bool
}

// TestConformance is the differential conformance harness: for every
// fixture in conformanceFixtures, instantiate the component on wazy with
// WithWASI configured from the fixture's args/env/fs, call
// wasi:cli/run@0.2.3#run, and assert stdout byte-for-byte matches the
// golden captured from wasmtime. Every fixture is run (not stopped at the
// first failure) so a single t.Logf tally at the end reports the true
// pass/fail count across the whole battery.
func TestConformance(t *testing.T) {
	pass, fail := 0, 0
	for _, fx := range conformanceFixtures {
		fx := fx
		t.Run(fx.name, func(t *testing.T) {
			ctx := context.Background()
			r := wazy.NewRuntime(ctx)
			defer r.Close(ctx)

			var stdin io.Reader
			if fx.stdin != nil {
				stdin = bytes.NewReader(fx.stdin)
			}

			var stdout, stderr bytes.Buffer
			inst, err := Instantiate(ctx, r, fx.wasm, WithWASI(WASIConfig{
				Stdout: &stdout,
				Stderr: &stderr,
				Stdin:  stdin,
				Args:   fx.args,
				Env:    fx.env,
				FS:     fx.fs,
			})...)
			if err != nil {
				t.Fatalf("Instantiate: %v", err)
			}
			defer inst.Close(ctx)

			results, callErr := inst.Call(ctx, "wasi:cli/run@0.2.3#run")

			if fx.wantCallErr {
				if callErr == nil {
					t.Errorf("Call run(): expected an error (guest calls wasi:cli/exit with a nonzero status), got nil; results=%v", results)
				} else {
					t.Logf("Call run() error (expected, exit path): %v", callErr)
				}
			} else if callErr != nil {
				t.Fatalf("Call run(): %v (stdout so far: %q, stderr so far: %q)", callErr, stdout.String(), stderr.String())
			} else {
				if len(results) != 1 {
					t.Fatalf("run() returned %d result(s), want 1", len(results))
				}
				rv, ok := results[0].(abi.ResultValue)
				if !ok {
					t.Fatalf("run() result: expected abi.ResultValue, got %T (%v)", results[0], results[0])
				}
				if rv.IsErr {
					t.Fatalf("run() returned Err, want Ok (stderr: %q)", stderr.String())
				}
			}

			got := stdout.String()
			if got == fx.golden {
				pass++
				t.Logf("PASS %s: stdout matches wasmtime golden exactly (%d bytes) -- %s", fx.name, len(got), fx.desc)
				return
			}
			fail++
			t.Errorf("FAIL %s: stdout does NOT match wasmtime golden -- %s\n--- wazy (%d bytes) ---\n%s\n--- wasmtime golden (%d bytes) ---\n%s",
				fx.name, fx.desc, len(got), got, len(fx.golden), fx.golden)
		})
	}
	t.Logf("conformance tally: %d/%d fixtures matched wasmtime byte-for-byte", pass, pass+fail)
}

//go:embed testdata/conformance/f01_arith.component.wasm
var f01ArithWasm []byte

//go:embed testdata/conformance/f01_arith.stdout.golden
var f01ArithGolden string

//go:embed testdata/conformance/f02_strings.component.wasm
var f02StringsWasm []byte

//go:embed testdata/conformance/f02_strings.stdout.golden
var f02StringsGolden string

//go:embed testdata/conformance/f03_args.component.wasm
var f03ArgsWasm []byte

//go:embed testdata/conformance/f03_args.stdout.golden
var f03ArgsGolden string

//go:embed testdata/conformance/f04_env.component.wasm
var f04EnvWasm []byte

//go:embed testdata/conformance/f04_env.stdout.golden
var f04EnvGolden string

//go:embed testdata/conformance/f05_collections.component.wasm
var f05CollectionsWasm []byte

//go:embed testdata/conformance/f05_collections.stdout.golden
var f05CollectionsGolden string

//go:embed testdata/conformance/f06_recursion.component.wasm
var f06RecursionWasm []byte

//go:embed testdata/conformance/f06_recursion.stdout.golden
var f06RecursionGolden string

//go:embed testdata/conformance/f07_fileread.component.wasm
var f07FilereadWasm []byte

//go:embed testdata/conformance/f07_fileread.stdout.golden
var f07FilereadGolden string

//go:embed testdata/conformance/f07_fileread.input.data.txt
var f07FilereadInput []byte

//go:embed testdata/conformance/f08_filewrite.component.wasm
var f08FilewriteWasm []byte

//go:embed testdata/conformance/f08_filewrite.stdout.golden
var f08FilewriteGolden string

//go:embed testdata/conformance/f09_exit.component.wasm
var f09ExitWasm []byte

//go:embed testdata/conformance/f09_exit.stdout.golden
var f09ExitGolden string

//go:embed testdata/conformance/f10_bigout.component.wasm
var f10BigoutWasm []byte

//go:embed testdata/conformance/f10_bigout.stdout.golden
var f10BigoutGolden string

//go:embed testdata/conformance/f11_floats.component.wasm
var f11FloatsWasm []byte

//go:embed testdata/conformance/f11_floats.stdout.golden
var f11FloatsGolden string

//go:embed testdata/conformance/f12_ints.component.wasm
var f12IntsWasm []byte

//go:embed testdata/conformance/f12_ints.stdout.golden
var f12IntsGolden string

//go:embed testdata/conformance/f13_json.component.wasm
var f13JsonWasm []byte

//go:embed testdata/conformance/f13_json.stdout.golden
var f13JsonGolden string

//go:embed testdata/conformance/f14_largedata.component.wasm
var f14LargedataWasm []byte

//go:embed testdata/conformance/f14_largedata.stdout.golden
var f14LargedataGolden string

//go:embed testdata/conformance/f15_deeprec.component.wasm
var f15DeeprecWasm []byte

//go:embed testdata/conformance/f15_deeprec.stdout.golden
var f15DeeprecGolden string

//go:embed testdata/conformance/f16_unicode.component.wasm
var f16UnicodeWasm []byte

//go:embed testdata/conformance/f16_unicode.stdout.golden
var f16UnicodeGolden string

//go:embed testdata/conformance/f17_multifs.component.wasm
var f17MultifsWasm []byte

//go:embed testdata/conformance/f17_multifs.stdout.golden
var f17MultifsGolden string

//go:embed testdata/conformance/f17_multifs.input.alpha.txt
var f17MultifsInputAlpha []byte

//go:embed testdata/conformance/f17_multifs.input.beta.txt
var f17MultifsInputBeta []byte

//go:embed testdata/conformance/f17_multifs.input.gamma.txt
var f17MultifsInputGamma []byte

//go:embed testdata/conformance/f18_panic.component.wasm
var f18PanicWasm []byte

//go:embed testdata/conformance/f18_panic.stdout.golden
var f18PanicGolden string

//go:embed testdata/conformance/f19_iterchains.component.wasm
var f19IterchainsWasm []byte

//go:embed testdata/conformance/f19_iterchains.stdout.golden
var f19IterchainsGolden string

//go:embed testdata/conformance/f20_cat.component.wasm
var f20CatWasm []byte

//go:embed testdata/conformance/f20_cat.stdout.golden
var f20CatGolden string

//go:embed testdata/conformance/f20_cat.input.stdin.data
var f20CatStdin []byte

//go:embed testdata/conformance/f21_wc.component.wasm
var f21WcWasm []byte

//go:embed testdata/conformance/f21_wc.stdout.golden
var f21WcGolden string

//go:embed testdata/conformance/f21_wc.input.stdin.data
var f21WcStdin []byte

//go:embed testdata/conformance/f22_grep.component.wasm
var f22GrepWasm []byte

//go:embed testdata/conformance/f22_grep.stdout.golden
var f22GrepGolden string

//go:embed testdata/conformance/f22_grep.input.stdin.data
var f22GrepStdin []byte

//go:embed testdata/conformance/f23_upperstdin.component.wasm
var f23UpperstdinWasm []byte

//go:embed testdata/conformance/f23_upperstdin.stdout.golden
var f23UpperstdinGolden string

//go:embed testdata/conformance/f23_upperstdin.input.stdin.data
var f23UpperstdinStdin []byte

//go:embed testdata/conformance/f24_regex.component.wasm
var f24RegexWasm []byte

//go:embed testdata/conformance/f24_regex.stdout.golden
var f24RegexGolden string

//go:embed testdata/conformance/f25_sha2.component.wasm
var f25Sha2Wasm []byte

//go:embed testdata/conformance/f25_sha2.stdout.golden
var f25Sha2Golden string

//go:embed testdata/conformance/f26_base64.component.wasm
var f26Base64Wasm []byte

//go:embed testdata/conformance/f26_base64.stdout.golden
var f26Base64Golden string

//go:embed testdata/conformance/f27_csv.component.wasm
var f27CsvWasm []byte

//go:embed testdata/conformance/f27_csv.stdout.golden
var f27CsvGolden string

//go:embed testdata/conformance/f28_itertools.component.wasm
var f28ItertoolsWasm []byte

//go:embed testdata/conformance/f28_itertools.stdout.golden
var f28ItertoolsGolden string

//go:embed testdata/conformance/f29_readdir.component.wasm
var f29ReaddirWasm []byte

//go:embed testdata/conformance/f29_readdir.stdout.golden
var f29ReaddirGolden string

//go:embed testdata/conformance/f30_filetypes.component.wasm
var f30FiletypesWasm []byte

//go:embed testdata/conformance/f30_filetypes.stdout.golden
var f30FiletypesGolden string

//go:embed testdata/conformance/f31_seek.component.wasm
var f31SeekWasm []byte

//go:embed testdata/conformance/f31_seek.stdout.golden
var f31SeekGolden string

//go:embed testdata/conformance/f32_nested.component.wasm
var f32NestedWasm []byte

//go:embed testdata/conformance/f32_nested.stdout.golden
var f32NestedGolden string

//go:embed testdata/conformance/f33_createlist.component.wasm
var f33CreatelistWasm []byte

//go:embed testdata/conformance/f33_createlist.stdout.golden
var f33CreatelistGolden string

//go:embed testdata/conformance/f34_append.component.wasm
var f34AppendWasm []byte

//go:embed testdata/conformance/f34_append.stdout.golden
var f34AppendGolden string

//go:embed testdata/conformance/f35_remove.component.wasm
var f35RemoveWasm []byte

//go:embed testdata/conformance/f35_remove.stdout.golden
var f35RemoveGolden string
