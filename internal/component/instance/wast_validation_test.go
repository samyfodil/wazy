package instance

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"path"
	"strings"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// wastValidationSuites are the validator suites, fetched by `make
// build.component_testdata` at the commits the Makefile pins, that
// TestWastValidation runs: the component-model specification's
// test/validation and test/binary suites, wasm-tools' instantiate and
// virtualize, and Wasmtime's restrictions and nested. Each directory holds
// one .wast and the manifest generated from it.
var wastValidationSuites = []string{
	"cm-validation-abi",
	"cm-validation-annotated-names",
	"cm-validation-core-modules",
	"cm-validation-defined-types",
	"cm-validation-extern-names",
	"cm-validation-external-visibility",
	"cm-validation-indicies",
	"cm-validation-instantiation",
	"cm-validation-kebab",
	"cm-validation-map",
	"cm-validation-outer-alias",
	"cm-validation-resources",
	"cm-binary",
	"wasm-tools-instantiate",
	"wasm-tools-virtualize",
	"wasmtime-restrictions",
	"wasmtime-nested",
}

// wastValidationGap is how wazy's answer to one command differs from the
// suite's: refused, with an error containing got, or -- got "" -- accepted.
type wastValidationGap struct {
	got string
	why string
}

// The reasons for the gaps.
const (
	// Wasmtime refuses every root-level component import; wazy runs one when
	// a component is supplied for it (WithComponentImport), and otherwise
	// refuses it for that.
	gapRootComponentImport = "wazy runs a root-level component import given WithComponentImport; without one it is refused as not supplied"
	// Wasmtime refuses a root component export; wazy accepts one, as it
	// does a root core module export -- a definition, with nothing to bind.
	gapRootComponentExport = "wazy accepts a root component export, which Wasmtime refuses"
	// A core module's code is validated by wazy's core validator, whose
	// messages are its own; the core specification's suites, which check the
	// same rules, compare no text.
	gapCoreWording = "wazy's core validator refuses it in its own words"
	// The suite expects the name as written lowercased; wasm-tools and
	// Wasmtime print it as written, and upstream's nyi.txt lists the suite as
	// not passing on Wasmtime.
	gapLowercasedName = "the suite expects the name lowercased, which no implementation prints"
	// An extern name with attributes -- `implements`, a version suffix --
	// is refused as a whole; decoding them is a change of its own.
	gapAttributeNames = "extern names with attributes are not decoded yet"
)

// wastValidationGaps is the EXACT set of commands wazy answers differently
// from the suite, per suite and file, with the reason. Like wastKnownSkips it
// is reconciled in both directions: a command answered differently that is
// not here fails, and so does an entry whose command is now answered as the
// suite says -- the gap closed, and the entry must go.
//
// Every check the suites make is answered as they say -- component-level and
// core-level alike, wasm-tools' words included. What is listed is where wazy
// deliberately differs from Wasmtime, whose restrictions suite pins what
// Wasmtime does not implement rather than what the component model forbids:
// wazy runs a root-level component import a component is supplied for, and
// accepts a root component export, both of which wasm-tools' validator
// accepts too.
var wastValidationGaps = map[string]map[string]wastValidationGap{
	"cm-binary": {
		// lines 1298, 1317: valid components importing names with attributes
		"binary.110.wasm": {"extern name with attributes (0x02)", gapAttributeNames},
		"binary.111.wasm": {"extern name with attributes (0x02)", gapAttributeNames},
		// line 1399: invalid leading byte (0x3) for name option
		"binary.115.wasm": {"extern name with attributes (0x02)", gapAttributeNames},
		// line 1497: duplicate 'implements' option in name
		"binary.123.wasm": {"extern name with attributes (0x02)", gapAttributeNames},
	},
	"cm-validation-kebab": {
		// line 160: import name `foob-ar` conflicts with previous name `foo-bar`
		"kebab.33.wasm": {"import name `FOOB-ar` conflicts with previous name `foo-bar`", gapLowercasedName},
	},
	"cm-validation-core-modules": {
		// line 25: type mismatch
		"core-modules.1.wasm": {"cannot pop the 1st operand for i32.add", gapCoreWording},
	},
	"wasmtime-restrictions": {
		// line 2: root-level component imports are not supported
		"restrictions.0.wasm": {"was not supplied; supply a component for it with WithComponentImport", gapRootComponentImport},
		// line 6: exporting a component from the root component is not supported
		"restrictions.1.wasm": {"", gapRootComponentExport},
		// line 17: root-level component imports are not supported
		"restrictions.3.wasm": {"was not supplied; supply a component for it with WithComponentImport", gapRootComponentImport},
	},
}

// TestWastValidation runs the vendored validator suites against wazy's
// component-level type checking (ctenv.go, through typeCheck): every
// `module` and `module_definition` must pass it, and -- when it has no
// imports and so can run -- instantiate; every `assert_invalid` must be
// refused, with an error containing the text the suite asserts. Where the
// static check passes an `assert_invalid`, it is instantiated, so a refusal
// at instantiation is seen for what it is.
func TestWastValidation(t *testing.T) {
	for _, suite := range wastValidationSuites {
		t.Run(suite, func(t *testing.T) { runWastValidationSuite(t, suite) })
	}
}

func runWastValidationSuite(t *testing.T, suite string) {
	dir := path.Join("testdata", "wast", suite)
	fetched(t, dir)
	manifests, err := fs.Glob(wastFS, path.Join(dir, "*.json"))
	if err != nil || len(manifests) != 1 {
		t.Fatalf("suite %s: want one manifest, found %v (%v)", suite, manifests, err)
	}
	raw, err := wastFS.ReadFile(manifests[0])
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Commands []wastCmd `json:"commands"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	gaps := wastValidationGaps[suite]
	seen := map[string]bool{}
	ran := 0
	for _, c := range manifest.Commands {
		switch c.Type {
		case "module", "module_definition", "assert_invalid":
		case "assert_malformed":
			if c.ModuleType == "text" {
				continue // a malformation of the text format, which wazy does not read
			}
		default:
			t.Fatalf("line %d: unexpected command %q", c.Line, c.Type)
		}
		wasm, err := wastFS.ReadFile(path.Join(dir, c.Filename))
		if err != nil {
			t.Fatal(err)
		}
		ran++
		err = wastValidate(ctx, r, wasm, c.Type)
		gap, isGap := gaps[c.Filename]
		seen[c.Filename] = isGap
		switch {
		case isGap && gap.got == "":
			if err != nil {
				t.Errorf("line %d: %s: %v; listed as accepted (%s)", c.Line, c.Filename, err, gap.why)
			}
		case isGap:
			if err == nil || !strings.Contains(err.Error(), gap.got) {
				t.Errorf("line %d: %s: %v; listed as refused with %q (%s)", c.Line, c.Filename, err, gap.got, gap.why)
			}
		case c.Type == "assert_invalid" || c.Type == "assert_malformed":
			if err == nil || !strings.Contains(err.Error(), c.Text) {
				t.Errorf("line %d: %s: %v, want it refused with %q", c.Line, c.Filename, err, c.Text)
			}
		default:
			if err != nil {
				t.Errorf("line %d: %s: %v", c.Line, c.Filename, err)
			}
		}
	}
	if ran == 0 {
		t.Fatalf("suite %s ran nothing", suite)
	}
	for f := range gaps {
		if !seen[f] {
			t.Errorf("suite %s: wastValidationGaps names %s, which the suite has no command for", suite, f)
		}
	}
}

// wastValidate is wazy's answer to one command: the decode and the type
// check, then the instantiation -- of a valid component only when it can run
// on its own, with no imports; of an invalid one always, since that is where
// wazy refuses what the type check does not, its imports included.
func wastValidate(ctx context.Context, r wazy.Runtime, wasm []byte, kind string) error {
	comp, err := binary.Decode(bytes.NewReader(wasm))
	if err != nil {
		return err
	}
	if err := typeCheck(comp, &config{}); err != nil {
		return err
	}
	if kind == "module_definition" || kind == "assert_malformed" || (kind == "module" && len(comp.Imports) > 0) {
		// Nothing supplies its imports, or it is malformed or not: the type
		// check, which reads every embedded core module, is all there is.
		return nil
	}
	in, err := Instantiate(ctx, r, wasm)
	if err != nil {
		return err
	}
	return in.Close(ctx)
}
