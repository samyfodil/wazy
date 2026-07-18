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
// guessed from the manifest's trap text alone). Five concrete bugs were
// found and fixed while building/hardening this harness -- see their
// commit-worthy doc comments at the call sites for detail:
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
//  4. composition.go's repToProviderHandle's StreamDesc/FutureDesc cases
//     expected the delegated arg to already BE the raw *sharedStream/
//     *sharedFuture identity, but the value actually flowing through a
//     guest<->guest delegated call (liftAsyncHostArgsPlanned's
//     resolveHandleArg, host_import.go) is a *StreamReader/*FutureReader --
//     the wrapper shape a real Go AsyncHostFunc consumes, built for the
//     "host is the other end" case, not this composition path. Compounding
//     that: repToProviderHandle also MINTED a handle in the provider's own
//     table right there (sub.resources.addEntry), but the provider's own
//     resolveArgHandles (instance.go), reached moments later via
//     sub.invoke's lowerParams (sync arm) or the async callee's
//     onStart->lowerParams (async arm), does that SAME minting itself from
//     the raw shared identity -- so pre-minting handed that second pass an
//     already-a-handle uint32 where it expects the *sharedStream, tripping
//     the exact "expected a *sharedStream, got uint32" trap. Fixed by (a)
//     unwrapping *StreamReader/*FutureReader to their .shared field and (b)
//     passing that raw shared identity straight through instead of
//     pre-minting -- mirroring how the sync arm's resolveArgHandles is the
//     ONE place a readable end is actually minted into the callee's table.
//     This is genuinely THE delegation type-mismatch bug named in every
//     "bug:"-labeled skip below; fixing it unblocks the args-crossing path
//     for both cancel-subtask and partial-stream-copies (each then hits its
//     own, separate, deeper gap -- see below) and is what makes cancel-
//     stream's $C.start-stream's stream RESULT (a plain sync delegated
//     call, different from these two suites' ARG-crossing shape but
//     exercising the same repToProviderHandle/providerHandleToRep pairing)
//     keep working.
//  5. stream_builtins.go's cancelCopyHostFunc (cancel_copy) skipped calling
//     shared.cancel() whenever e.hasPendingEvent() was already true --
//     correct when that pending event is FINAL (an immediate completion, or
//     a buffer-exhausted rendezvous that already reset the shared object's
//     own bookkeeping), but streamCopyHostFunc's onCopy closure (a peer's
//     rendezvous making PARTIAL progress against a still-open buffer, e.g.
//     a 4-byte write satisfying part of a 100-byte pending read) ALSO sets
//     e.hasPendingEvent() true, for a notification the shared object still
//     considers LIVE/reclaimable (sharedStream.write/read's `pendingBuffer.
//     remain() > 0` branch never resets pending_buffer/pending_on_copy_done
//     -- only the peer's OWN onCopyDone does). A cancel racing that live
//     notification was silently swallowing it and replaying the stale
//     COMPLETED event instead of superseding it with CANCELLED, e.g.
//     cancel-stream.wast's "call $write4 [4 bytes into a 100-byte pending
//     read]; cancel-read; expect CANCELLED|(4<<4)" got COMPLETED|(4<<4)
//     instead -- confirmed root-caused via instrumented tracing (call/
//     return logging added and removed during investigation), independent
//     of and unrelated to fix #4 above (this suite never crosses a
//     delegation boundary with a stream/future ARG at all -- start-stream's
//     stream is a plain sync RESULT). Fixed by adding streamEnd.livePending
//     (set true by onCopy, false by onCopyDone, reset false at the top of
//     every fresh copy) and having cancelCopyHostFunc call shared.cancel()
//     whenever `!hasPending() || livePending()`; futureEnd has no such flag
//     since future.{read,write} copy exactly one element atomically (no
//     partial-progress notion, so no onCopy call exists to race). Verified
//     against async_scenarios.json's stream-cancel-read oracle scenario
//     (an uncontested cancel, unaffected since livePending() is always
//     false there) and the full oracle/resources/multiple-resources .wast
//     suites -- all stay green. cancel-stream now passes.
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
//   - cancel-subtask: fixes #2 and #4 land Instantiate AND get $D.run past
//     both delegated calls' arg/result crossing (the "f" call -- callback-
//     based async lift, subtask.cancel dance -- fully passes now). It then
//     traps calling $C's "g" (`(func (export "g") async (param "fut" $FT)
//     (result u32)) (canon lift (core func $cm "g")))` -- an async lift
//     declared async at the TYPE level but with NO `callback` option on its
//     `canon lift`, i.e. a "stackful" export whose core code blocks
//     synchronously via waitable-set.wait mid-body with no callback
//     re-entry point). A top-level (non-delegated) call to such an export
//     dispatches through plain sync invoke() (instance.go's
//     `if be.asyncCallback {...}` gate on boundExport.asyncCallback, false
//     here). But startAsyncExportTask (async_lift.go), used UNCONDITIONALLY
//     by every guest<->guest delegated async lower regardless of whether
//     the target actually has a callback, always drives the target through
//     guestTask's callback-loop machinery (guest_task.go's firstRun calls
//     be.coreFn.CallWithStack then interprets stack[0] as a PACKED
//     CALLBACK-LOOP CODE) -- correct only for a callback-based lift. For a
//     no-callback target this is doubly wrong: it would misinterpret the
//     export's real i32 result as a packed code if the call ever returned,
//     and -- what actually fires first here -- the export's inline
//     waitable-set.wait blocks for real (waiting on a future D's OWN code
//     will only write AFTER this delegated call returns), which can only be
//     satisfied by driving the shared scheduler's run queue
//     (async_builtins.go); since D's own task is synchronously stuck inside
//     this very call (wazy has no true fiber/stack-switch), nothing can
//     make progress, and the SAME deadlock guard sync-streams/zero-length
//     already hit reports "an import resolved externally requires
//     CallAsync (not yet implemented)". This is the identical deferred
//     CallAsync/true-stackful-coroutine gap, reached through a THIRD door
//     (a no-callback async export invoked across a guest<->guest
//     delegation boundary) -- confirmed by re-running after fix #4 landed
//     (delegation itself no longer errors; this is what surfaces next).
//   - partial-stream-copies: fixes #2 and #4 land Instantiate and the
//     stream ARG crossing itself (no more "delegated stream arg" error).
//     $D.run then traps directly (not inside a callee) on its OWN
//     waitable-set.wait with "called outside an active async task" --
//     $D's "run" export (like cancel-subtask's "g" above) is an async-TYPED
//     `canon lift` with no `callback` option, so the top-level call
//     dispatches through plain sync invoke() (no guestTask, no active task
//     recorded), and invoke()'s core code then calls the waitable-set.wait
//     builtin directly from that task-less sync context. Exactly the same
//     missing synchronous-task-may-not-block tracking as dont-block-start/
//     empty-wait/deadlock (see their entries below) -- a pre-existing gap
//     fix #4 exposes rather than causes, confirmed unrelated to the
//     delegation fix by re-running after #4 landed (the delegated stream
//     arg no longer errors; this is what surfaces next, before $D.run ever
//     reaches a delegated call).
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
//     blocking-call chain. Left unfixed (still pre-skipped this session,
//     deliberately, so it can never hang go test ./...); NOT re-attempted
//     after fixes #4/#5 landed (both are stream/composition-path fixes,
//     unrelated to sched.drive/guestTask park-resume, and this suite must
//     stay hard-gated behind a real timeout even to reproduce -- see the
//     skip's own text). Root cause NARROWED (a later session): the earlier
//     "no-callback stackful lift" lead is DISPROVEN -- async-calls-sync.wast's
//     lifts (blocking-call/unblock/sync-func) all carry `async (callback ...)`,
//     so it is not the stackful gap. It is also NOT unbounded recursion: a
//     temporary re-entry-depth guard on startAsyncExportTask (trap past depth
//     512) did NOT fire before the 15s timeout, so the Go stack stays bounded
//     -- confirming a genuine LIVELOCK (sched.drive spins, step() keeps
//     returning progressed=true, but the awaited condition never converges),
//     not a deadlock the trap should catch. The distinguishing feature is
//     CONCURRENCY: the .wast's own header says $AsyncOuter.run "asynchronously
//     calls sync-func twice CONCURRENTLY", and the two concurrent subtasks +
//     the blocking-call/unblock handshake form a cycle the single-threaded
//     FIFO scheduler does not drive to convergence. Fixing it needs the
//     concurrent-task convergence model (the same deferred CallAsync/parked-
//     concurrency territory), not a sched.drive tweak -- so it stays a real
//     bug whose fix is gated behind a deferred feature, hard-pre-skipped so it
//     can never hang go test ./....
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
	{name: "async-calls-sync", skipReason: "deferred: NARROWED post-stackful-lift (docs/component-model-async-stackful-design.md §4.4 implemented and verified) -- run1 (both $SyncMiddle instances, sync-opts STACKFUL lift) now converges correctly and returns 42 (confirmed in isolation, timeout-guarded). run2 substitutes $AsyncMiddle for one of the two sync-func targets: $AsyncMiddle.sync-func is a plain CALLBACK lift (asyncCallback, not stackful) whose own core body sync-lowers to blocking-call BEFORE ever calling task.return, so its own first synchronous run blocks inside a nested sched.drive that can only converge if ITS OWN sync caller (AsyncOuter2.run) keeps running -- the design doc's acknowledged honest-flag #1 (§12): a callback task blocking on its own sync caller needs callback-task parking, not addressed here. Exact same class of gap as sync-streams/zero-length, reached through run2's $AsyncMiddle door; hard timeout-guarded, never hangs go test ./..."},
	{name: "big-interleaving-test", skipReason: "deferred: stream/subtask busy-state protocol traps (concurrent-op guard, busy-drop) not implemented -- see asyncSuite doc"},
	{name: "builtin-trap-poisons-instance", skipReason: "deferred: instance-poisoning-after-trap ('cannot enter component instance') not implemented -- wazy's mayEnter guard is transient, cleared by leaveRun on every exit path including traps"},
	{name: "cancellable", skipReason: "deferred: decode fails, 'async canon kind 0xc not yet supported' -- pre-existing, documented out-of-scope decoder gap, unrelated to this session's async work"},
	{name: "cancel-stream"},
	{name: "cancel-subtask"},
	{name: "closed-stream"},
	{name: "cross-abi-calls", skipReason: "deferred: its largest-arity signatures hit graph.go's pre-existing, explicit 'cross-instance realloc is not supported' limitation"},
	{name: "cross-task-future"},
	{name: "deadlock"},
	{name: "dont-block-start"},
	{name: "drop-cross-task-borrow", skipReason: "deferred: cross-task borrow-handle leak trap ('borrow handles still remain at the end of the call') not implemented"},
	{name: "drop-stream", skipReason: "deferred: busy-stream drop trap not implemented"},
	{name: "drop-subtask", skipReason: "deferred: unresolved-subtask drop trap not implemented"},
	{name: "drop-waitable-set", skipReason: "deferred: waitable-set-with-waiters drop trap not implemented"},
	{name: "empty-wait"},
	{name: "futures-must-write", skipReason: "deferred: future write-end must-write-before-drop trap not implemented"},
	{name: "partial-stream-copies"},
	{name: "passing-resources", skipReason: "deferred: handle-table lifecycle trap ('unknown handle index') for this cross-task-transfer shape not implemented"},
	{name: "same-component-stream-future", skipReason: "deferred: intra-component read/write-own-stream trap not implemented"},
	{name: "sync-barges-in", skipReason: "deferred: decode fails, 'async canon kind 0xc not yet supported' -- same pre-existing decoder gap as cancellable"},
	{name: "sync-streams", skipReason: "deferred: NOT a stackful-lift gap -- $D.run (sync-opts stackful) sync-lowers to $C.get/set, both CALLBACK lifts (asyncCallback); $C.get's core code calls task.return then, in the SAME core call (before ever returning a packed callback code), calls stream.write directly, which blocks. That nested sched.drive runs on $D's own stackful goroutine but can never converge: the only thing that could complete the rendezvous is $D's OWN continuation past this very sync-lowered call, which is frame-held beneath the nested drive. This is the design doc's acknowledged honest-flag #1 (docs/component-model-async-stackful-design.md §12): a CALLBACK task blocking on its own sync caller needs the callback task's WAIT/blocking sites run through stackfulTask-style parking too, which this design explicitly does not wire up -- a real, separate, deferred generalization, not a stackful-lift-of-the-top-level-export gap"},
	{name: "trap-if-block-and-sync", skipReason: "deferred: verified opportunistically post-stackful-lift -- the sync-block-trap assertions themselves are now correct (blockingTask), but the fixture also exercises real thread.suspend/thread.new-indirect/thread.index/thread.yield canon builtins (a genuine multi-fiber thread API, out of scope -- same pre-existing decoder gap noted for cancellable/sync-barges-in's 'async canon kind 0xc') and stream/future .cancel-read/-write canons; needs that thread-builtin support before this suite can run at all, unrelated to the stackful-lift design"},
	{name: "trap-if-done", skipReason: "deferred: future/stream double-read/write-after-done trap state machine not implemented"},
	{name: "trap-if-sync-and-waitable-set", skipReason: "deferred: waitable-in-set-used-synchronously trap not implemented"},
	{name: "trap-if-transfer-in-waitable-set", skipReason: "deferred: lift-while-enqueued-in-waitable-set trap not implemented"},
	{name: "trap-on-reenter", skipReason: "deferred: instance-poisoning-after-trap (same gap as builtin-trap-poisons-instance)"},
	{name: "validate-no-async-abi-for-sync-type", skipReason: "deferred: no decode/bind-time cross-check between a func type's async-ness and its canon lift/lower's async CanonOpt"},
	{name: "validate-no-stream-char"},
	{name: "wait-during-callback"},
	{name: "zero-length", skipReason: "deferred: same class of gap as sync-streams -- $Parent sync-lowers to $Producer.produce (a CALLBACK lift); produce parks at WAIT via the callback loop waiting for $Consumer to write, but $Parent (the only thing that could ever drive $Consumer forward) is itself frame-held inside invokeAsyncCallback's nested sched.drive for this very sync-lowered call to produce. Design doc §12 honest-flag #1: a callback task blocking on its own sync caller's continuation needs callback-task parking, not addressed by this design"},
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
