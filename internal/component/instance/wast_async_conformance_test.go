package instance

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// newWastRuntime builds the runtime a suite runs on. WAZY_REPRO_INTERPRETER=1
// swaps the native engine for the interpreter, which is what tells an
// engine-independent bug apart from one in generated code -- see TODOS.md's
// "## OPEN BUG" (the open Windows teardown corruption) and the
// win-corruption-repro workflow's interpreter shard, which has reproduced that
// crash with the JIT out of the picture. Unset (every normal run, CI included)
// this is exactly wazy.NewRuntime.
func newWastRuntime(ctx context.Context) wazy.Runtime {
	if os.Getenv("WAZY_REPRO_INTERPRETER") == "1" {
		return wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigInterpreter())
	}
	return wazy.NewRuntime(ctx)
}

// TestAsyncWastConformance runs the official WebAssembly/component-model
// ASYNC conformance suites (test/async/*.wast) through wazy, mirroring
// TestWastConformance (wast_conformance_test.go) but for the richer command
// set the async suites use: module_definition/module_instance (the
// multi-component linking model these suites use to re-instantiate one
// definition under a fresh name before each assertion) alongside the sync
// harness's module/assert_return/assert_trap, plus assert_invalid
// (decode/bind must fail) and assert_uninstantiable (Instantiate must fail
// with a matching trap-text substring). Every suite is run, with none
// skipped, and its assertions checked strictly: a trap's text must match too.
//
// module_instance semantics (confirmed by inspecting every vendored
// manifest, not assumed): every module_instance command is immediately
// followed by the assert_return/assert_trap/action commands that target it;
// no command in any suite carries an explicit instance-selector
// field, and none wire one instance's exports as another's imports (no
// manifest anywhere carries an "imports" key). So "current instance" tracking
// -- identical in spirit to the sync harness's `in` -- is sufficient; the
// richer named-instance map exists only so module_definition/module_instance
// round-trip through storage exactly as the manifest models them.
func TestAsyncWastConformance(t *testing.T) {
	suites := asyncWastSuites(t)
	pass, fail := 0, 0
	for _, s := range suites {
		t.Run(s, func(t *testing.T) {
			if runAsyncWastSuite(t, s) {
				pass++
			} else {
				fail++
			}
		})
	}
	t.Logf("async wast conformance: %d pass / %d fail (of %d suites)", pass, fail, len(suites))
}

// asyncWastSuites names every suite under testdata/wast-async: the
// directory holds exactly the component-model commit's test/async, so a suite
// is added or retired by moving that commit, not by editing a list.
func asyncWastSuites(t *testing.T) []string {
	t.Helper()
	requireFetched(t, wastAsyncFS, "testdata/wast-async/*/*.json")
	ents, err := fs.ReadDir(wastAsyncFS, "testdata/wast-async")
	if err != nil {
		t.Fatal(err)
	}
	var suites []string
	for _, e := range ents {
		if e.IsDir() {
			suites = append(suites, e.Name())
		}
	}
	return suites
}

// runAsyncWastSuite drives one suite's manifest and reports whether every
// command in it was satisfied (true) or something genuinely mismatched
// (false, with the mismatches logged via t.Error by the caller's subtest).
// It never t.Fatals mid-suite on a semantic mismatch -- only a malformed
// fixture (unreadable file, unparsable JSON) is fatal, since that's a
// harness bug rather than a conformance result.
func runAsyncWastSuite(t *testing.T, suite string) bool {
	dir := path.Join("testdata", "wast-async", suite)
	requireFetched(t, wastAsyncFS, dir)
	raw, err := wastAsyncFS.ReadFile(path.Join(dir, suite+".json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest struct {
		Commands []wastCmd `json:"commands"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}

	ctx := context.Background()
	r := newWastRuntime(ctx)
	defer r.Close(ctx)

	defs := map[string][]byte{} // module_definition name -> wasm bytes
	var opened []*Instance
	defer func() {
		for _, inst := range opened {
			inst.Close(ctx)
		}
	}()

	var current *Instance // the instance assert_return/assert_trap/action target
	ok := true
	assertsRun := 0

	readWasm := func(filename string, line int) ([]byte, bool) {
		wasm, err := wastAsyncFS.ReadFile(path.Join(dir, filename))
		if err != nil {
			t.Errorf("line %d: read %s: %v", line, filename, err)
			ok = false
			return nil, false
		}
		return wasm, true
	}

	instantiate := func(wasm []byte, line int) *Instance {
		inst, err := Instantiate(ctx, r, wasm)
		if err != nil {
			t.Errorf("line %d: instantiate: %v", line, err)
			ok = false
			return nil
		}
		opened = append(opened, inst)
		return inst
	}

	// WAZY_REPRO_MAX_CMDS=N stops the suite after N manifest commands, so the
	// open Windows corruption (TODOS.md) can be bisected down to the command
	// that first makes it reachable. The teardown assertion never fires, so
	// the damage predates Close; narrowing by command is what is left. Unset
	// (every normal run) this runs the whole manifest.
	maxCmds := len(manifest.Commands)
	if v := os.Getenv("WAZY_REPRO_MAX_CMDS"); v != "" {
		if n, convErr := strconv.Atoi(v); convErr == nil && n >= 0 {
			maxCmds = n
		}
	}

	// WAZY_REPRO_ONLY_PAIR=k runs command 0 (the module_definition, which
	// every instance needs) plus ONLY the k'th module_instance/assert_trap
	// pair, 1-based. MAX_CMDS alone cannot separate "the k'th assertion is
	// what breaks" from "k live instances is what breaks", because each pair
	// instantiates and nothing is closed until the suite ends. This isolates
	// one assertion against exactly one instance.
	onlyPair := 0
	if v := os.Getenv("WAZY_REPRO_ONLY_PAIR"); v != "" {
		if n, convErr := strconv.Atoi(v); convErr == nil && n > 0 {
			onlyPair = n
		}
	}

	// WAZY_REPRO_PAIR_REPEAT=n, with ONLY_PAIR set, runs that one pair n times
	// instead of once. No single pair crashes in isolation but three different
	// pairs do, so the open question is whether the trigger is how MANY live
	// instances have accumulated or WHICH assertions ran. Repeating one
	// harmless pair answers it: a crash means pure instance count.
	pairRepeat := 1
	if v := os.Getenv("WAZY_REPRO_PAIR_REPEAT"); v != "" {
		if n, convErr := strconv.Atoi(v); convErr == nil && n > 1 {
			pairRepeat = n
		}
	}

	cmds := manifest.Commands
	if onlyPair > 0 && 2*onlyPair < len(cmds) {
		sel := []wastCmd{cmds[0]}
		for r := 0; r < pairRepeat; r++ {
			sel = append(sel, cmds[2*onlyPair-1], cmds[2*onlyPair])
		}
		cmds = sel
	}

	for ci, c := range cmds {
		if ci >= maxCmds {
			break
		}
		switch c.Type {
		case "module_definition":
			wasm, readOK := readWasm(c.Filename, c.Line)
			if !readOK {
				continue
			}
			defs[c.Name] = wasm

		case "module_instance":
			wasm, haveDef := defs[c.Module]
			if !haveDef {
				t.Errorf("line %d: module_instance %s: unknown module_definition %q", c.Line, c.Instance, c.Module)
				ok = false
				current = nil
				continue
			}
			current = instantiate(wasm, c.Line)

		case "module":
			wasm, readOK := readWasm(c.Filename, c.Line)
			if !readOK {
				continue
			}
			current = instantiate(wasm, c.Line)

		case "assert_invalid":
			wasm, readOK := readWasm(c.Filename, c.Line)
			if !readOK {
				continue
			}
			assertsRun++
			_, decErr := binary.Decode(bytes.NewReader(wasm))
			bindErr := decErr
			if decErr == nil {
				// Decode alone accepted it; some invalid shapes (e.g. an
				// async/sync canon-opt mismatch) are only a bind-time
				// concern -- give Instantiate a chance to reject it too
				// before calling this a miss.
				inst, err := Instantiate(ctx, r, wasm)
				if inst != nil {
					inst.Close(ctx)
				}
				bindErr = err
			}
			if bindErr == nil {
				t.Errorf("line %d: assert_invalid: expected decode/bind failure (%q), got success", c.Line, c.Text)
				ok = false
			} else if !strings.Contains(bindErr.Error(), c.Text) {
				t.Errorf("line %d: assert_invalid: error = %q, want substring %q", c.Line, bindErr.Error(), c.Text)
				ok = false
			}

		case "assert_uninstantiable":
			wasm, readOK := readWasm(c.Filename, c.Line)
			if !readOK {
				continue
			}
			assertsRun++
			inst, err := Instantiate(ctx, r, wasm)
			if inst != nil {
				inst.Close(ctx)
			}
			if err == nil {
				t.Errorf("line %d: assert_uninstantiable: expected failure (%q), got success", c.Line, c.Text)
				ok = false
			} else if !strings.Contains(err.Error(), c.Text) {
				t.Errorf("line %d: assert_uninstantiable: error = %q, want substring %q", c.Line, err.Error(), c.Text)
				ok = false
			}

		case "assert_return", "assert_trap", "action":
			if c.Action == nil || c.Action.Type != "invoke" {
				continue
			}
			if current == nil {
				t.Errorf("line %d: %s: no current instance (a prior module/module_instance failed)", c.Line, c.Action.Field)
				ok = false
				continue
			}
			assertsRun++
			got, err := invokeWast(ctx, current, c.Action.Field, c.Action.Args)
			switch c.Type {
			case "assert_trap":
				if err == nil {
					t.Errorf("line %d: %s: expected trap %q, got success", c.Line, c.Action.Field, c.Text)
					ok = false
				} else if !strings.Contains(err.Error(), c.Text) {
					t.Errorf("line %d: %s: trap = %q, want substring %q", c.Line, c.Action.Field, err.Error(), c.Text)
					ok = false
				}

			case "action":
				// A bare invoke with no expected result/trap -- only its
				// completion (no error) matters.
				if err != nil {
					t.Errorf("line %d: %s: unexpected error: %v", c.Line, c.Action.Field, err)
					ok = false
				}

			default: // assert_return
				if err != nil {
					t.Errorf("line %d: %s: unexpected error: %v", c.Line, c.Action.Field, err)
					ok = false
					continue
				}
				want := expectedWast(t, current, c.Action.Field, c.Expected)
				mismatch := len(got) != len(want)
				for i := 0; !mismatch && i < len(want); i++ {
					mismatch = !reflect.DeepEqual(got[i], want[i])
				}
				if mismatch {
					t.Errorf("line %d: %s = %#v, want %#v", c.Line, c.Action.Field, got, want)
					ok = false
				}
			}
		}
	}

	if assertsRun == 0 {
		t.Errorf("suite %s ran zero assertions", suite)
		ok = false
	}
	return ok
}
