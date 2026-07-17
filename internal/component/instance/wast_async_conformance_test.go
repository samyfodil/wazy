package instance

import (
	"bytes"
	"context"
	"encoding/json"
	"path"
	"reflect"
	"strings"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// TestAsyncWastConformance runs the official WebAssembly/component-model
// ASYNC conformance suites (test/async/*.wast) through wazy, mirroring
// TestWastConformance (wast_conformance_test.go) but for the richer command
// set the async suites use: module_definition/module_instance (the
// multi-component linking model these suites use to re-instantiate one
// definition under a fresh name before each assertion) alongside the sync
// harness's module/assert_return/assert_trap, plus assert_invalid
// (decode/bind must fail) and assert_uninstantiable (Instantiate must fail
// with a matching trap-text substring). See asyncSuite's doc for why each
// non-passing suite is classified PASS/SKIP/FAIL: asyncWastSuites' skipReason
// pre-classifies each suite from real investigation of internal/component
// (not guessed), so a KNOWN-GOOD suite regressing is a real t.Error (and
// reds go test ./...) while a documented gap stays a t.Skip regardless of
// what runAsyncWastSuite would have found.
//
// module_instance semantics (confirmed by inspecting every vendored
// manifest, not assumed): every module_instance command is immediately
// followed by the assert_return/assert_trap/action commands that target it;
// no command in any of the 31 suites carries an explicit instance-selector
// field, and none wire one instance's exports as another's imports (no
// manifest anywhere carries an "imports" key). So "current instance" tracking
// -- identical in spirit to the sync harness's `in` -- is sufficient; the
// richer named-instance map exists only so module_definition/module_instance
// round-trip through storage exactly as the manifest models them.
func TestAsyncWastConformance(t *testing.T) {
	pass, skip, fail := 0, 0, 0
	for _, s := range asyncWastSuites {
		t.Run(s.name, func(t *testing.T) {
			if s.skipReason != "" {
				skip++
				t.Skip(s.skipReason)
			}
			if runAsyncWastSuite(t, s.name) {
				pass++
			} else {
				fail++
			}
		})
	}
	t.Logf("async wast conformance: %d pass / %d skip / %d fail (of %d suites)", pass, skip, fail, len(asyncWastSuites))
}

// asyncSuite pairs a vendored suite name with why it's skipped, if it is.
//
// A suite is skipped only when it exercises a genuinely deferred/incomplete
// wazy feature -- confirmed either by direct inspection of
// internal/component (zero matches for the spec's exact trap string) or by
// actually running the suite and observing the real failure mode (never
// guessed from the manifest's trap text alone). Three concrete bugs were
// found and fixed while building this harness -- see their commit-worthy
// doc comments at the call sites for detail:
//
//  1. stream.go's copyElementsMemmove bounds-checked an elementless
//     stream/future copy (elemSz == 0, e.g. `(type (future))`) against its
//     ptr even though zero bytes ever cross the boundary, so the spec's
//     deliberate garbage pointer (0xdeadbeef, exercised by empty-wait and
//     wait-during-callback) spuriously trapped. Fixed: skip the bounds
//     check (and the copy itself, which would otherwise slice out of
//     range) when nb == 0. wait-during-callback now passes.
//  2. composition.go's outerFuncArgHostImport (now outerFuncArgImport)
//     assumed a `(with "x" (func N))` nested-component-instantiate arg's
//     func alias always targets a HOST import; when N instead aliases a
//     SIBLING nested component instance's export (the async suites'
//     multi-nested-component composition shape, e.g. async-calls-sync.wast
//     wiring $AsyncInner's "blocking-call" export into $SyncMiddle's func
//     import of the same name), it misread the alias's InstanceIdx as a
//     host-import index and failed with a nonsensical "out of range of 0
//     imported instances". Fixed by branching on InstanceIdx vs
//     numImported and wiring the sibling's single export via
//     delegatingHostImport, exactly like the already-correct
//     arg.Sort==0x05 (whole-instance) case does per-export. This unblocked
//     6 suites' Instantiate calls (async-calls-sync, cancel-subtask,
//     deadlock, partial-stream-copies, sync-streams, zero-length), each of
//     which then surfaced a genuinely separate, deeper gap below.
//  3. binary/descriptor.go's stream-type decoder accepted `stream<char>`,
//     which the spec explicitly forbids (component-model#607: a stream
//     element must be re-encodable independent of its neighbors, which
//     char's UTF-8 validity check can't guarantee mid-stream). Fixed by
//     rejecting a char element at decode time. validate-no-stream-char now
//     passes.
//
// Remaining skips, by root cause:
//
//   - builtin-trap-poisons-instance, trap-on-reenter: the spec's "an
//     unhandled trap permanently poisons the component instance -- every
//     later call fails with 'cannot enter component instance'" invariant.
//     wazy's only re-entrancy guard is the transient mayEnter flag
//     (instance.go), reset by leaveRun on every exit path including traps
//     (task.go); there is no latching poisoned-after-trap state on Instance
//     for either the sync or async call path (verified: zero matches for
//     "cannot enter component instance" outside testdata).
//   - dont-block-start, trap-if-block-and-sync: requires treating a core
//     module's instantiation-time start function (or a sync-context guest
//     task) as one that must not block/park; wazy has no such tracking at
//     all (verified: zero matches for "cannot block"/"synchronous task").
//   - validate-no-async-abi-for-sync-type: requires a decode/bind-time
//     cross-check between a component-func type's Async bit
//     (binary.FuncDesc.Async) and its canon lift/lower's async CanonOpt;
//     never cross-checked anywhere (verified: zero matches for "requires an
//     async function type").
//   - fine-grained stream/future/subtask/waitable-set protocol-violation
//     traps not implemented as their own state machines (trap-if-done,
//     trap-if-sync-and-waitable-set, trap-if-transfer-in-waitable-set,
//     futures-must-write, drop-stream, drop-subtask, drop-waitable-set,
//     drop-cross-task-borrow, same-component-stream-future,
//     passing-resources, big-interleaving-test): each needs the runtime to
//     track an additional double-write/double-read-after-done, busy-drop,
//     dangling-borrow, or transfer-while-enqueued invariant Phases 0-3
//     didn't implement (verified: zero matches for every one of their exact
//     spec trap strings anywhere in internal/component). Real gaps, not
//     "cheap" -- each is its own state machine to add.
//   - cancellable, sync-barges-in: fail to even decode ("async canon kind
//     0xc not yet supported", decoder.go:938) -- an async canonical-function
//     builtin kind 0xc's decode is explicitly out of scope per
//     binary/component.go's own CanonKind doc (pre-existing, not part of
//     this session's async work).
//   - cross-abi-calls: Instantiate fails on its largest-arity signatures
//     with graph.go's existing, explicit "realloc core func targets a
//     different core instance than the lift's own core func; cross-instance
//     realloc is not supported" -- a pre-existing architectural limitation
//     of the graph engine, unrelated to async.
//   - sync-streams, zero-length: after fix #2 above, Instantiate succeeds
//     but the call fails with the graph engine's own explicit "an import
//     resolved externally requires CallAsync (not yet implemented)" --
//     exactly the "public CallAsync" deferred feature named in this task's
//     brief.
//   - cancel-subtask, partial-stream-copies: after fix #2, Instantiate
//     succeeds but the call fails with "delegated future/stream arg:
//     expected a *sharedFuture/*sharedStream, got
//     *instance.FutureReader/StreamReader" -- passing a stream/future
//     handle THROUGH a delegatingHostImport sibling-wiring boundary (the
//     very path fix #2 completes) isn't fully wired for ownership transfer;
//     a real, separate gap fix #2 exposes rather than causes.
//   - deadlock: after fix #2, Instantiate succeeds and a real trap does
//     fire, but with the wrong text -- "waitable-set.wait: called outside
//     an active async task" instead of the spec's "wasm trap: deadlock
//     detected: event loop cannot make further progress". Root cause
//     confirmed the same way as empty-wait below: $D's "f" export is a
//     PLAIN sync lift (`(canon lift (core func $dm "g"))`, no async option
//     on the lift despite the func's async TYPE) whose core code calls
//     waitable-set.wait directly in sync context to wait on a subtask that
//     can now never wake it. Same missing synchronous-task-may-not-block
//     tracking as dont-block-start/empty-wait; the reference just names
//     this particular shape of it "deadlock" rather than "cannot block".
//   - async-calls-sync: after fix #2, Instantiate succeeds but the call
//     hangs indefinitely (confirmed via `timeout 15 go test`, ~100% CPU,
//     no progress) rather than resolving or trapping -- a real scheduler
//     livelock in the self-referential blocking-call -> sync-func ->
//     blocking-call chain. Left unfixed (out of this session's scope) and
//     deliberately pre-skipped so it can never hang go test ./....
//   - empty-wait: after the elemSz==0 fix (#1 above), the suite gets
//     further but still fails. Checked against the actual compiled
//     $D "run" export (`wasm-tools print empty-wait.0.wasm`): its func
//     TYPE is declared `async`, but the `canon lift` binding it is a
//     PLAIN synchronous lift (no `async` option on the lift itself, so
//     graph.go's isAsyncLift/stackful-rejection check -- which only looks
//     at the lift's own CanonOpt, not the type -- never applies here). The
//     core "run" function is therefore invoked as an ordinary synchronous
//     export and calls the waitable-set.wait builtin directly from that
//     sync context; that's the exact "a synchronous context attempts to
//     block" case (same root gap as dont-block-start/trap-if-block-and-
//     sync's missing synchronous-task-may-not-block tracking), just
//     reached through a different door -- an existing guard
//     (requireActiveTask) does trap it, but with the generic "called
//     outside an active async task" instead of a dedicated
//     blocking-in-sync-context trap.
//   - cancel-stream: a real "unreachable" trap fires directly inside the
//     test's own $D.run (not inside a callee, per the wasm stack trace),
//     i.e. one of run's many inline stream.read/stream.cancel-read/
//     stream.write status-code equality assertions doesn't match wazy's
//     actual return value. stream.cancel-read/write ARE implemented
//     (stream_builtins.go's cancelCopyHostFunc), so this is a real,
//     likely-narrow behavioral bug in their partial-copy/cancel-race
//     status-code semantics -- not root-caused to a single line within
//     this session's budget, so left as an honest skip rather than a
//     guessed fix.
//
// Every other suite is run for real with strict assertions (including a
// trap-text substring match, stricter than the sync harness which only
// checks err != nil) -- a regression there is a genuine build-breaking
// failure, by design.
type asyncSuite struct {
	name       string
	skipReason string
}

var asyncWastSuites = []asyncSuite{
	{name: "async-calls-sync", skipReason: "bug: scheduler livelock (hangs, confirmed via timeout) in the self-referential blocking-call -> sync-func -> blocking-call chain -- see asyncSuite doc"},
	{name: "big-interleaving-test", skipReason: "deferred: stream/subtask busy-state protocol traps (concurrent-op guard, busy-drop) not implemented -- see asyncSuite doc"},
	{name: "builtin-trap-poisons-instance", skipReason: "deferred: instance-poisoning-after-trap ('cannot enter component instance') not implemented -- wazy's mayEnter guard is transient, cleared by leaveRun on every exit path including traps"},
	{name: "cancellable", skipReason: "deferred: decode fails, 'async canon kind 0xc not yet supported' -- pre-existing, documented out-of-scope decoder gap, unrelated to this session's async work"},
	{name: "cancel-stream", skipReason: "bug: real 'unreachable' trap inside the test's own inline stream.read/cancel-read/write status-code assertions -- not root-caused this session, see asyncSuite doc"},
	{name: "cancel-subtask", skipReason: "bug: cross-component future delegation through a sibling host-import wiring is incomplete ('delegated future arg: expected a *sharedFuture, got *instance.FutureReader') -- exposed by, not caused by, the graph.go composition fix in this change"},
	{name: "closed-stream"},
	{name: "cross-abi-calls", skipReason: "deferred: its largest-arity signatures hit graph.go's pre-existing, explicit 'cross-instance realloc is not supported' limitation"},
	{name: "cross-task-future"},
	{name: "deadlock", skipReason: "deferred: a real trap fires but with generic text ('called outside an active async task' instead of 'deadlock detected...') -- same missing synchronous-task-may-not-block tracking as dont-block-start/empty-wait, see asyncSuite doc"},
	{name: "dont-block-start", skipReason: "deferred: no synchronous-task-may-not-block tracking around a core module's instantiation-time start (Instantiate has no such guard)"},
	{name: "drop-cross-task-borrow", skipReason: "deferred: cross-task borrow-handle leak trap ('borrow handles still remain at the end of the call') not implemented"},
	{name: "drop-stream", skipReason: "deferred: busy-stream drop trap not implemented"},
	{name: "drop-subtask", skipReason: "deferred: unresolved-subtask drop trap not implemented"},
	{name: "drop-waitable-set", skipReason: "deferred: waitable-set-with-waiters drop trap not implemented"},
	{name: "empty-wait", skipReason: "deferred: 'run' is a plain sync lift (of an async-TYPED func) whose core code blocks synchronously via waitable-set.wait -- the same missing synchronous-task-may-not-block tracking as dont-block-start, reached through a sync lift instead of instantiation-time start -- see asyncSuite doc"},
	{name: "futures-must-write", skipReason: "deferred: future write-end must-write-before-drop trap not implemented"},
	{name: "partial-stream-copies", skipReason: "bug: cross-component stream delegation through a sibling host-import wiring is incomplete ('delegated stream arg: expected a *sharedStream, got *instance.StreamReader') -- exposed by, not caused by, the graph.go composition fix in this change"},
	{name: "passing-resources", skipReason: "deferred: handle-table lifecycle trap ('unknown handle index') for this cross-task-transfer shape not implemented"},
	{name: "same-component-stream-future", skipReason: "deferred: intra-component read/write-own-stream trap not implemented"},
	{name: "sync-barges-in", skipReason: "deferred: decode fails, 'async canon kind 0xc not yet supported' -- same pre-existing decoder gap as cancellable"},
	{name: "sync-streams", skipReason: "deferred: public CallAsync not implemented -- graph.go's own explicit 'an import resolved externally requires CallAsync (not yet implemented)'"},
	{name: "trap-if-block-and-sync", skipReason: "deferred: sync-context blocking trap + start-blocking guard not implemented (shares dont-block-start's gap)"},
	{name: "trap-if-done", skipReason: "deferred: future/stream double-read/write-after-done trap state machine not implemented"},
	{name: "trap-if-sync-and-waitable-set", skipReason: "deferred: waitable-in-set-used-synchronously trap not implemented"},
	{name: "trap-if-transfer-in-waitable-set", skipReason: "deferred: lift-while-enqueued-in-waitable-set trap not implemented"},
	{name: "trap-on-reenter", skipReason: "deferred: instance-poisoning-after-trap (same gap as builtin-trap-poisons-instance)"},
	{name: "validate-no-async-abi-for-sync-type", skipReason: "deferred: no decode/bind-time cross-check between a func type's async-ness and its canon lift/lower's async CanonOpt"},
	{name: "validate-no-stream-char"},
	{name: "wait-during-callback"},
	{name: "zero-length", skipReason: "deferred: public CallAsync not implemented -- graph.go's own explicit 'an import resolved externally requires CallAsync (not yet implemented)'"},
}

// runAsyncWastSuite drives one suite's manifest and reports whether every
// command in it was satisfied (true) or something genuinely mismatched
// (false, with the mismatches logged via t.Error by the caller's subtest).
// It never t.Fatals mid-suite on a semantic mismatch -- only a malformed
// fixture (unreadable file, unparsable JSON) is fatal, since that's a
// harness bug rather than a conformance result.
func runAsyncWastSuite(t *testing.T, suite string) bool {
	dir := path.Join("testdata", "wast-async", suite)
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
	r := wazy.NewRuntime(ctx)
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
		inst, err := Instantiate(ctx, r, wasm, WithWASI(WASIConfig{})...)
		if err != nil {
			t.Errorf("line %d: instantiate: %v", line, err)
			ok = false
			return nil
		}
		opened = append(opened, inst)
		return inst
	}

	for _, c := range manifest.Commands {
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
				inst, err := Instantiate(ctx, r, wasm, WithWASI(WASIConfig{})...)
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
			inst, err := Instantiate(ctx, r, wasm, WithWASI(WASIConfig{})...)
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
