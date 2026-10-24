package instance

import (
	"bytes"
	"context"
	"encoding/json"
	"path"
	"strings"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// wastValidationSuites are the vendored validator suites (see each one's
// README) that TestWastValidation runs: the component-model specification's
// test/validation instantiation and resources suites, wasm-tools'
// instantiate and virtualize, and Wasmtime's restrictions and nested. The
// directory's name ends in the name of its .wast.
var wastValidationSuites = []string{
	"cm-validation-instantiation",
	"cm-validation-resources",
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
	// The component-level type check (ctenv.go) does not model core module
	// types: wazy runs no component that hands a core module to another (see
	// ctEntitySub), so the type a core module is passed or exported as is
	// not checked.
	gapCoreModuleTypes = "core module types are not checked: wazy runs no component passing a core module to another"
	// wazy does not support a component importing a core module, and
	// refuses one at instantiation for that, before the core instantiation
	// the suite checks is reached.
	gapCoreModuleImport = "a core module import is refused at instantiation, before the core instantiation is reached"
	// Core instantiation is checked by wazy's core linker when it
	// instantiates the module -- and refused there, in its own words.
	gapCoreLinker = "core instantiation is checked by wazy's core linker, worded its own way"
	// Core-level validation a validator does and wazy does not: the core
	// alias or the destructor is never used.
	gapCoreValidation = "core-level validation wazy does not do: the core alias or destructor is never used"
	// wazy does not run a component that exports a core module, aliases one
	// out of an instance, or passes one to a nested component.
	gapCoreModuleRuntime = "wazy does not run a component that exports, aliases or passes a core module"
	// The decoder refuses a core module type declaring a non-func import or
	// export (readCoreImportdesc).
	gapCoreModuleTypeDecode = "the decoder refuses a core module type with a non-func import or export"
	// Wasmtime refuses every root-level component import; wazy runs one when
	// a component is supplied for it (WithComponentImport), and otherwise
	// refuses it for that.
	gapRootComponentImport = "wazy runs a root-level component import given WithComponentImport; without one it is refused as not supplied"
	// wazy refuses a root component export, in its own words.
	gapRootComponentExport = "wazy refuses a root component export, worded its own way"
	// wazy refuses a root func import at instantiation, before the
	// re-export of it Wasmtime refuses is reached.
	gapRootFuncImport = "a root func import is refused at instantiation, before its re-export is reached"
)

// wastValidationGaps is the EXACT set of commands wazy answers differently
// from the suite, per suite and file, with the reason. Like wastKnownSkips it
// is reconciled in both directions: a command answered differently that is
// not here fails, and so does an entry whose command is now answered as the
// suite says -- the gap closed, and the entry must go.
//
// Every component-level type check the suites make is answered as they say;
// what is listed is core-level, or a deliberate difference from Wasmtime.
var wastValidationGaps = map[string]map[string]wastValidationGap{
	"cm-validation-instantiation": {
		// line 298: type mismatch in import `::f`
		"instantiation.33.wasm": {"", gapCoreModuleTypes},
		// line 306: missing expected import `::extra`
		"instantiation.34.wasm": {"", gapCoreModuleTypes},
		// line 314: missing expected export `x`
		"instantiation.35.wasm": {"", gapCoreModuleTypes},
		// line 322: type mismatch in export `g`
		"instantiation.36.wasm": {"", gapCoreModuleTypes},
		// line 357: expected: (func)
		"instantiation.39.wasm": {"import func[wazy:component/empty-import.f]: signature mismatch: v_v != i32_v", gapCoreLinker},
		// line 365: expected: (func)
		"instantiation.40.wasm": {"import func[wazy:component/empty-import.f]: signature mismatch: v_v != v_i32", gapCoreLinker},
		// line 373: expected: (func)
		"instantiation.41.wasm": {"import func[wazy:component/empty-import.f]: signature mismatch: v_v != i32_v", gapCoreLinker},
		// line 382: expected global type i32, found i64
		"instantiation.42.wasm": {"import global[wazy:component/empty-import.g]: value type mismatch: i32 != i64", gapCoreLinker},
		// line 390: expected table element type funcref, found externref
		"instantiation.43.wasm": {"import table[wazy:component/empty-import.t]: table type mismatch: funcref != externref", gapCoreLinker},
		// line 398: mismatch in table limits
		"instantiation.44.wasm": {"import table[wazy:component/empty-import.t]: minimum size mismatch: 2 > 1", gapCoreLinker},
		// line 406: mismatch in table limits
		"instantiation.45.wasm": {"import table[wazy:component/empty-import.t]: maximum size mismatch: 2, but actual has no max", gapCoreLinker},
		// line 414: mismatch in table limits
		"instantiation.46.wasm": {"import table[wazy:component/empty-import.t]: maximum size mismatch: 2 < 3", gapCoreLinker},
		// line 422: mismatch in the shared flag for memories
		"instantiation.47.wasm": {"discover import types: import[0] memory[wazy:component/empty-import.m]: shared memory requested but threads feature not enabled", gapCoreLinker},
		// line 430: mismatch in memory limits
		"instantiation.48.wasm": {"import memory[wazy:component/empty-import.m]: minimum size mismatch: 1 > 0", gapCoreLinker},
		// line 438: expected global, found func
		"instantiation.49.wasm": {"export \"b\" in module \"\" is a func, not a global", gapCoreLinker},
		// line 449: missing module instantiation argument
		"instantiation.50.wasm": {"instantiate embedded core module: import[0] has an empty module name", gapCoreLinker},
		// line 455: does not export an item named `table`
		"instantiation.51.wasm": {"\"table\" is not exported in module \"\"", gapCoreLinker},
		// line 638: core instance 0 has no export named `a`
		"instantiation.78.wasm": {"", gapCoreValidation},
	},
	"cm-validation-resources": {
		// line 751: wrong signature for a destructor
		"resources.60.wasm": {"", gapCoreValidation},
	},
	"wasm-tools-instantiate": {
		// line 158: missing module instantiation argument
		"instantiate.13.wasm": {"is a core module import", gapCoreModuleImport},
		// line 201: type mismatch in import `::`
		"instantiate.18.wasm": {"was not supplied", gapCoreModuleTypes},
		// line 215: missing expected import `::foobar`
		"instantiate.19.wasm": {"was not supplied", gapCoreModuleTypes},
		// line 226: missing expected export `x`
		"instantiate.20.wasm": {"was not supplied", gapCoreModuleTypes},
		// line 236: (valid)
		"instantiate.21.wasm": {"core:importdesc: unsupported sort 0x3", gapCoreModuleTypeDecode},
		// line 286: expected: (func)
		"instantiate.24.wasm": {"is a core module import", gapCoreModuleImport},
		// line 294: expected: (func)
		"instantiate.25.wasm": {"is a core module import", gapCoreModuleImport},
		// line 302: expected global type i32, found i64
		"instantiate.26.wasm": {"is a core module import", gapCoreModuleImport},
		// line 310: expected table element type funcref, found externref
		"instantiate.27.wasm": {"is a core module import", gapCoreModuleImport},
		// line 318: mismatch in table limits
		"instantiate.28.wasm": {"is a core module import", gapCoreModuleImport},
		// line 326: mismatch in table limits
		"instantiate.29.wasm": {"is a core module import", gapCoreModuleImport},
		// line 334: mismatch in table limits
		"instantiate.30.wasm": {"is a core module import", gapCoreModuleImport},
		// line 342: mismatch in the shared flag for memories
		"instantiate.31.wasm": {"is a core module import", gapCoreModuleImport},
		// line 350: mismatch in memory limits
		"instantiate.32.wasm": {"is a core module import", gapCoreModuleImport},
		// line 358: type mismatch in export `g`
		"instantiate.33.wasm": {"is a core module import", gapCoreModuleImport},
		// line 373: (valid)
		"instantiate.35.wasm": {"which only an instantiation supplies", gapCoreModuleRuntime},
		// line 451: expected global, found func
		"instantiate.47.wasm": {"export \"\" in module \"\" is a func, not a global", gapCoreLinker},
		// line 584: module instantiation argument `` does not export an item named `table`
		"instantiate.60.wasm": {"is a core module import", gapCoreModuleImport},
	},
	"wasmtime-restrictions": {
		// line 2: root-level component imports are not supported
		"restrictions.0.wasm": {"was not supplied; supply a component for it with WithComponentImport", gapRootComponentImport},
		// line 6: exporting a component from the root component is not supported
		"restrictions.1.wasm": {"only func and instance exports are supported", gapRootComponentExport},
		// line 10: component export `f` is a reexport of an imported function which is not implemented
		"restrictions.2.wasm": {"is a func import (sort 0x1)", gapRootFuncImport},
		// line 17: root-level component imports are not supported
		"restrictions.3.wasm": {"was not supplied; supply a component for it with WithComponentImport", gapRootComponentImport},
	},
	"wasmtime-nested": {
		// line 24: (valid)
		"nested.3.wasm": {"which only an instantiation supplies", gapCoreModuleRuntime},
		// line 36: (valid)
		"nested.4.wasm": {"which only an instantiation supplies", gapCoreModuleRuntime},
		// line 70: (valid)
		"nested.5.wasm": {"unsupported sort 0x0", gapCoreModuleRuntime},
		// line 100: (valid)
		"nested.6.wasm": {"unsupported sort 0x0", gapCoreModuleRuntime},
		// line 128: (valid)
		"nested.7.wasm": {"only func and instance exports are supported", gapCoreModuleRuntime},
		// line 149: (valid)
		"nested.8.wasm": {"core:importdesc: unsupported sort 0x3", gapCoreModuleTypeDecode},
		// line 175: (valid)
		"nested.9.wasm": {"core:importdesc: unsupported sort 0x3", gapCoreModuleTypeDecode},
		// line 213: (valid)
		"nested.10.wasm": {"only func and instance exports are supported", gapCoreModuleRuntime},
		// line 219: (valid)
		"nested.11.wasm": {"core:importdesc: unsupported sort 0x3", gapCoreModuleTypeDecode},
		// line 264: (valid)
		"nested.14.wasm": {"which only an instantiation supplies", gapCoreModuleRuntime},
		// line 322: (valid)
		"nested.16.wasm": {"which only an instantiation supplies", gapCoreModuleRuntime},
		// line 374: (valid)
		"nested.17.wasm": {"which only an instantiation supplies", gapCoreModuleRuntime},
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
	stem := suite[strings.LastIndexByte(suite, '-')+1:]
	raw, err := wastFS.ReadFile(path.Join(dir, stem+".json"))
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
		case c.Type == "assert_invalid":
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
	if kind == "module_definition" || (kind == "module" && len(comp.Imports) > 0) {
		return nil // nothing supplies its imports: the type check is all there is
	}
	in, err := Instantiate(ctx, r, wasm)
	if err != nil {
		return err
	}
	return in.Close(ctx)
}
