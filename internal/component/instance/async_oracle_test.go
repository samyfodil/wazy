package instance

// This file is the Go half of the async differential oracle described in
// docs/component-model-async-oracle-design.md: it reads the SAME scenario
// battery gen_async_oracle.py interprets against the vendored reference
// implementation (testdata/definitions.py), interprets it against wazy's
// real async builtins/runtime -- direct hostFuncDef calls on a hand-built
// Instance/task, the same pattern as async_builtins_test.go/stream_test.go/
// waitable_test.go, no wasm needed for the builtin surface -- and deep-diffs
// the resulting trace against async_oracle_golden.json.
//
// To regenerate the golden file after editing async_scenarios.json or
// updating the vendored definitions.py:
//
//	python3 internal/component/abi/testdata/gen_async_oracle.py
//
// async_scenarios.json is the single contract both languages build from, so
// there is no risk of the two batteries drifting apart independently. Both
// JSON files are embedded from package abi (abi/async_oracle_data.go) rather
// than duplicated here or read via a relative path: Go's //go:embed cannot
// cross a package directory boundary ("../abi/testdata/..." is not a legal
// pattern), so exporting the bytes from the package that already owns
// testdata/ is what keeps a single source of truth while still giving this
// test the CWD-independent guarantee package abi's own oracle tests document
// (oracle_embed_test.go: CI's scratch/BSD jobs run the compiled test binary
// from the repo root).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// ---- async_scenarios.json schema ----

type asyncScenariosFile struct {
	Scenarios []asyncScenario `json:"scenarios"`
}

type asyncScenario struct {
	Name           string           `json:"name"`
	Desc           string           `json:"desc"`
	TaskResult     *string          `json:"task_result"`
	Ops            []map[string]any `json:"ops"`
	GoTrapContains string           `json:"go_trap_contains"`
}

// ---- async_oracle_golden.json schema ----

type asyncGoldenFile struct {
	ScenariosSHA256 string                         `json:"scenarios_sha256"`
	Scenarios       map[string]asyncGoldenScenario `json:"scenarios"`
}

type asyncGoldenScenario struct {
	Trace []asyncTraceEntry `json:"trace"`
	Table []asyncTableEntry `json:"table"`
}

// asyncTraceEntry is one op's observable effect (§2 of the design doc). Only
// the fields relevant to Kind are ever populated on either side (both the
// golden decode and the Go-constructed "got" value) so a straight
// reflect.DeepEqual is meaningful: e.g. Vals is non-nil (possibly empty) for
// "ret"/"task-resolve", and left nil for "event"/"mem"/"trap".
type asyncTraceEntry struct {
	Op        int      `json:"op"`
	Kind      string   `json:"kind"`
	Vals      []uint32 `json:"vals,omitempty"`
	Code      uint32   `json:"code,omitempty"`
	P1        uint32   `json:"p1,omitempty"`
	P2        uint32   `json:"p2,omitempty"`
	Cancelled bool     `json:"cancelled,omitempty"`
	Bytes     string   `json:"bytes,omitempty"`
	Deadlock  bool     `json:"deadlock,omitempty"`
}

// asyncTableEntry is one live handle-table slot in the scenario's final
// snapshot (§2 footer). State is only meaningful when Kind == "subtask";
// Copy only when Kind is one of the stream/future end kinds -- for every
// other kind both fields sit at their Go zero value on both the golden
// decode and the Go-constructed side, so they compare equal without needing
// a presence flag.
type asyncTableEntry struct {
	Index uint32 `json:"index"`
	Kind  string `json:"kind"`
	State int    `json:"state,omitempty"`
	Copy  string `json:"copy,omitempty"`
}

// ---- harness scratch-memory layout (must match gen_async_oracle.py's
// EVENT_PTR/IMPORT_RETPTR_BASE/IMPORT_RETPTR_STRIDE exactly). ----
const (
	asyncEventPtr           = 0x100
	asyncImportRetptrBase   = 0x200
	asyncImportRetptrStride = 16
)

func loadAsyncOracleData(t *testing.T) (asyncScenariosFile, asyncGoldenFile) {
	t.Helper()
	var sf asyncScenariosFile
	if err := json.Unmarshal(abi.AsyncScenariosJSON, &sf); err != nil {
		t.Fatalf("parsing async_scenarios.json: %v", err)
	}
	if len(sf.Scenarios) == 0 {
		t.Fatal("async_scenarios.json battery is empty")
	}

	var gf asyncGoldenFile
	if err := json.Unmarshal(abi.AsyncOracleGoldenJSON, &gf); err != nil {
		t.Fatalf("parsing async_oracle_golden.json: %v", err)
	}

	// Staleness hash (§3 regeneration discipline): async_scenarios.json must
	// have been the exact input gen_async_oracle.py last ran against, or
	// this whole test is comparing against a stale golden silently.
	sum := sha256.Sum256(abi.AsyncScenariosJSON)
	got := hex.EncodeToString(sum[:])
	if got != gf.ScenariosSHA256 {
		t.Fatalf("async_scenarios.json has changed since the golden was generated (sha256 %s, golden recorded %s); "+
			"rerun: python3 internal/component/abi/testdata/gen_async_oracle.py", got, gf.ScenariosSHA256)
	}
	return sf, gf
}

func TestAsyncOracle(t *testing.T) {
	sf, gf := loadAsyncOracleData(t)

	for _, sc := range sf.Scenarios {
		sc := sc
		t.Run(sc.Name, func(t *testing.T) {
			want, ok := gf.Scenarios[sc.Name]
			if !ok {
				t.Fatalf("%s: no golden entry (did you run gen_async_oracle.py?)", sc.Name)
			}
			gotTrace, gotTable := runAsyncOracleScenario(t, sc)
			diffAsyncTrace(t, sc.Name, gotTrace, want.Trace)
			if !reflect.DeepEqual(gotTable, want.Table) {
				t.Errorf("%s: final table = %+v, want %+v", sc.Name, gotTable, want.Table)
			}
		})
	}
}

// diffAsyncTrace compares got against want entry-by-entry with a message
// naming the scenario, op index, and field -- not just a bulk DeepEqual
// failure -- per the design doc's "a mismatch is a test failure naming the
// scenario+op" requirement (§0).
func diffAsyncTrace(t *testing.T, name string, got, want []asyncTraceEntry) {
	t.Helper()
	n := len(got)
	if len(want) > n {
		n = len(want)
	}
	mismatch := false
	for i := 0; i < n; i++ {
		if i >= len(got) {
			t.Errorf("%s: trace entry %d: missing (want %+v)", name, i, want[i])
			mismatch = true
			continue
		}
		if i >= len(want) {
			t.Errorf("%s: trace entry %d: unexpected (got %+v)", name, i, got[i])
			mismatch = true
			continue
		}
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("%s: trace entry %d (op %d): got %+v, want %+v", name, i, want[i].Op, got[i], want[i])
			mismatch = true
		}
	}
	if mismatch {
		t.Logf("%s: full got trace: %+v", name, got)
		t.Logf("%s: full want trace: %+v", name, want)
	}
}

// ---- op-arg decoding helpers ----

func opStr(op map[string]any, key string) string {
	s, _ := op[key].(string)
	return s
}

func opBool(op map[string]any, key string, def bool) bool {
	if v, ok := op[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

func opInt(op map[string]any, key string) int {
	f, _ := op[key].(float64)
	return int(f)
}

func opIntDefault(op map[string]any, key string, def int) int {
	if v, ok := op[key]; ok {
		if f, ok := v.(float64); ok {
			return int(f)
		}
	}
	return def
}

func opHasKey(op map[string]any, key string) bool {
	_, ok := op[key]
	return ok
}

// resolveHandle mirrors gen_async_oracle.py's resolve_handle: {"$":"name"}
// resolves from env; a bare number is used raw (deliberately, for
// stale/wrong-index trap scenarios); 0 for waitable.join's "leave set" arm.
func resolveHandle(env map[string]uint32, raw any) uint32 {
	switch v := raw.(type) {
	case map[string]any:
		name, _ := v["$"].(string)
		h, ok := env[name]
		if !ok {
			panic(fmt.Sprintf("component/instance: async oracle: scenario bug: unbound handle name %q", name))
		}
		return h
	case float64:
		return uint32(v)
	default:
		panic(fmt.Sprintf("component/instance: async oracle: scenario bug: bad handle ref %#v", raw))
	}
}

func bind(env map[string]uint32, op map[string]any, val uint32) {
	if as, ok := op["as"].(string); ok {
		env[as] = val
	}
}

// deferAsyncRounds chains n AsyncCall.Defer hops before fn runs -- the Go
// side's realization of a "resolve_after: n" import (§3 point 5's documented
// asymmetry: this is n real scheduler run-queue hops, not n literal driver
// rounds the way the Python side counts them; only op-boundary results are
// pinned, not internal step counts).
func deferAsyncRounds(call *AsyncCall, n int, fn func()) {
	if n <= 1 {
		call.Defer(fn)
		return
	}
	call.Defer(func() { deferAsyncRounds(call, n-1, fn) })
}

// deferSchedRounds is deferAsyncRounds' host.cancel-root counterpart: not
// tied to any one AsyncCall, so it chains directly on the instance
// scheduler.
func deferSchedRounds(s *sched, n int, fn func()) {
	if n <= 1 {
		s.enqueue(func() error { fn(); return nil })
		return
	}
	s.enqueue(func() error { deferSchedRounds(s, n-1, fn); return nil })
}

// buildImportAsyncFn implements the scenario's import.call "behavior"
// vocabulary (design doc §1.3) as an AsyncHostFunc -- the Go-side twin of
// gen_async_oracle.py's make_import_callee.
func buildImportAsyncFn(behavior string, resolveAfter int, result *uint32) AsyncHostFunc {
	vals := func() []abi.Value {
		if result == nil {
			return nil
		}
		return []abi.Value{*result}
	}
	return func(_ context.Context, _ []abi.Value, call *AsyncCall) error {
		switch behavior {
		case "cancel-resolves-cancelled":
			call.OnCancel(func() { call.ResolveCancelled() })
		case "cancel-completes":
			call.OnCancel(func() { call.Resolve(vals()) })
		case "never":
			// no OnCancel, no Resolve: never resolves.
		case "ignore-cancel":
			call.OnCancel(func() {})
			deferAsyncRounds(call, resolveAfter, func() { call.Resolve(vals()) })
		default: // "resolve"
			if resolveAfter == 0 {
				call.Resolve(vals())
			} else {
				deferAsyncRounds(call, resolveAfter, func() { call.Resolve(vals()) })
			}
		}
		return nil
	}
}

// runAsyncOracleScenario interprets sc's op sequence against a hand-built
// Instance/task (the async_builtins_test.go/stream_test.go pattern), calling
// the SAME hostFuncDef closures the real graph engine wires -- no wasm
// needed for the builtin surface, real linear memory via memModule(t) for
// mem.write/mem.check/import retptrs/event out-structs. Returns (nil, nil)
// as its table iff the scenario trapped (matching the golden's "table":
// null convention, §2).
func runAsyncOracleScenario(t *testing.T, sc asyncScenario) (trace []asyncTraceEntry, table []asyncTableEntry) {
	t.Helper()
	ctx, mod := memModule(t)

	in := &Instance{
		sched:     &sched{},
		mayLeave:  true,
		mayEnter:  true,
		resources: newHandleTable(),
		resolve:   func(uint32) binary.TypeDesc { return nil },
	}
	currentOp := -1
	tk := &task{inst: in, gt: &guestTask{}, state: taskStarted}
	tk.onResolve = func(vals []abi.Value, cancelled bool) {
		v := make([]uint32, len(vals))
		for i, val := range vals {
			v[i] = val.(uint32)
		}
		trace = append(trace, asyncTraceEntry{Op: currentOp, Kind: "task-resolve", Cancelled: cancelled, Vals: v})
	}
	in.activeTask = tk

	env := map[string]uint32{}
	importOrdinal := 0
	elemOf := map[uint32]string{} // stream/future handle (either end) -> its "elem" scenario field ("" for bare)

	appendRet := func(op int, vals []uint32) {
		if vals == nil {
			vals = []uint32{}
		}
		trace = append(trace, asyncTraceEntry{Op: op, Kind: "ret", Vals: vals})
	}

	var taskReturnCanon binary.Canon
	if sc.TaskResult != nil {
		ref := binary.TypeRef{Primitive: *sc.TaskResult}
		taskReturnCanon = binary.Canon{TaskReturnResult: binary.FuncResults{Unnamed: &ref}}
	}
	taskReturnDef, err := taskReturnHostFuncGraph(in, taskReturnCanon)
	if err != nil {
		t.Fatalf("%s: taskReturnHostFuncGraph: %v", sc.Name, err)
	}
	taskCancelDef := taskCancelHostFuncGraph(in)

	step := func(k int, op map[string]any) {
		kind := opStr(op, "op")
		switch kind {
		case "waitable-set.new":
			def := waitableSetNewHostFunc(in)
			stack := []uint64{0}
			def.fn.Call(ctx, mod, stack)
			h := uint32(stack[0])
			bind(env, op, h)
			appendRet(k, []uint32{h})

		case "waitable-set.wait", "waitable-set.poll":
			si := resolveHandle(env, op["set"])
			cancellable := opBool(op, "cancellable", false)
			var def hostFuncDef
			if kind == "waitable-set.wait" {
				def = waitableSetWaitHostFunc(in, binary.Canon{Cancellable: cancellable})
			} else {
				def = waitableSetPollHostFunc(in, binary.Canon{Cancellable: cancellable})
			}
			stack := []uint64{uint64(si), asyncEventPtr}
			def.fn.Call(ctx, mod, stack)
			code := uint32(stack[0])
			p1, _ := mod.Memory().ReadUint32Le(asyncEventPtr)
			p2, _ := mod.Memory().ReadUint32Le(asyncEventPtr + 4)
			trace = append(trace, asyncTraceEntry{Op: k, Kind: "event", Code: code, P1: p1, P2: p2})

		case "waitable-set.drop":
			def := waitableSetDropHostFunc(in)
			si := resolveHandle(env, op["set"])
			def.fn.Call(ctx, mod, []uint64{uint64(si)})
			appendRet(k, nil)

		case "waitable.join":
			def := waitableJoinHostFunc(in)
			wi := resolveHandle(env, op["w"])
			si := resolveHandle(env, op["set"])
			def.fn.Call(ctx, mod, []uint64{uint64(wi), uint64(si)})
			appendRet(k, nil)

		case "import.call":
			var resultPtr *uint32
			if opHasKey(op, "result") {
				r := uint32(opInt(op, "result"))
				resultPtr = &r
			}
			behavior := opStr(op, "behavior")
			if behavior == "" {
				behavior = "resolve"
			}
			resolveAfter := opIntDefault(op, "resolve_after", 0)
			asyncFn := buildImportAsyncFn(behavior, resolveAfter, resultPtr)
			hi := &hostImport{asyncFn: asyncFn}
			if resultPtr != nil {
				hi.results = []binary.TypeDesc{binary.PrimitiveDesc{Prim: "u32"}}
			}
			fn, params, results, werr := buildAsyncHostWrapper(in, "oracle", "call", hi, in.resources, mod, nil)
			if werr != nil {
				t.Fatalf("%s: op %d: buildAsyncHostWrapper: %v", sc.Name, k, werr)
			}
			// The core calling convention reuses one stack buffer for both
			// params and results (api.GoModuleFunction); an import.call with
			// no declared result has zero params but still writes one i32
			// result, so size the buffer to the larger of the two.
			stackLen := len(params)
			if len(results) > stackLen {
				stackLen = len(results)
			}
			stack := make([]uint64, stackLen)
			if resultPtr != nil {
				retptr := uint32(asyncImportRetptrBase + asyncImportRetptrStride*importOrdinal)
				importOrdinal++
				stack[len(stack)-1] = uint64(retptr)
			}
			fn.Call(ctx, mod, stack)
			packed := uint32(stack[0])
			state := packed & 0xF
			if state != uint32(subtaskReturned) {
				bind(env, op, packed>>4)
			}
			appendRet(k, []uint32{packed})

		case "subtask.cancel":
			i := resolveHandle(env, op["sub"])
			async := opBool(op, "async", false)
			def := subtaskCancelHostFuncGraph(in, binary.Canon{Async: async})
			stack := []uint64{uint64(i)}
			def.fn.Call(ctx, mod, stack)
			appendRet(k, []uint32{uint32(stack[0])})

		case "subtask.drop":
			def := subtaskDropHostFunc(in)
			i := resolveHandle(env, op["sub"])
			def.fn.Call(ctx, mod, []uint64{uint64(i)})
			appendRet(k, nil)

		case "task.return":
			var vals []any
			if v, ok := op["vals"].([]any); ok {
				vals = v
			}
			stack := make([]uint64, len(taskReturnDef.params))
			if len(vals) == 1 {
				stack[0] = uint64(uint32(vals[0].(float64)))
			}
			taskReturnDef.fn.Call(ctx, mod, stack)
			appendRet(k, nil)

		case "task.cancel":
			taskCancelDef.fn.Call(ctx, mod, nil)
			appendRet(k, nil)

		case "host.cancel-root":
			after := opInt(op, "after")
			deferSchedRounds(in.sched, after, func() {
				if rerr := tk.requestCancellation(); rerr != nil {
					panic(fmt.Errorf("component/instance: async oracle: host.cancel-root: %w", rerr))
				}
			})
			appendRet(k, nil)

		case "context.get":
			slot := uint32(opInt(op, "slot"))
			def, cerr := contextGetHostFuncGraph(in, binary.Canon{CoreValType: 0x7f, Slot: slot})
			if cerr != nil {
				t.Fatalf("%s: op %d: contextGetHostFuncGraph: %v", sc.Name, k, cerr)
			}
			stack := []uint64{0}
			def.fn.Call(ctx, mod, stack)
			appendRet(k, []uint32{uint32(stack[0])})

		case "context.set":
			slot := uint32(opInt(op, "slot"))
			val := uint32(opInt(op, "val"))
			def, cerr := contextSetHostFuncGraph(in, binary.Canon{CoreValType: 0x7f, Slot: slot})
			if cerr != nil {
				t.Fatalf("%s: op %d: contextSetHostFuncGraph: %v", sc.Name, k, cerr)
			}
			def.fn.Call(ctx, mod, []uint64{uint64(val)})
			appendRet(k, nil)

		case "backpressure.inc":
			backpressureIncHostFuncGraph(in).fn.Call(ctx, mod, nil)
			appendRet(k, nil)

		case "backpressure.dec":
			backpressureDecHostFuncGraph(in).fn.Call(ctx, mod, nil)
			appendRet(k, nil)

		case "stream.new":
			elemName := opStr(op, "elem")
			elemDesc, elemSz, align, numeric := asyncElemDesc(elemName)
			def := streamNewHostFunc(in, elemDesc, elemSz, align, numeric)
			stack := []uint64{0}
			def.fn.Call(ctx, mod, stack)
			ri, wi := uint32(stack[0]), uint32(stack[0]>>32)
			elemOf[ri], elemOf[wi] = elemName, elemName
			if as, ok := op["as_read"].(string); ok {
				env[as] = ri
			}
			if as, ok := op["as_write"].(string); ok {
				env[as] = wi
			}
			appendRet(k, []uint32{ri, wi})

		case "future.new":
			elemName := opStr(op, "elem")
			elemDesc, elemSz, align, numeric := asyncElemDesc(elemName)
			def := futureNewHostFunc(in, elemDesc, elemSz, align, numeric)
			stack := []uint64{0}
			def.fn.Call(ctx, mod, stack)
			ri, wi := uint32(stack[0]), uint32(stack[0]>>32)
			elemOf[ri], elemOf[wi] = elemName, elemName
			if as, ok := op["as_read"].(string); ok {
				env[as] = ri
			}
			if as, ok := op["as_write"].(string); ok {
				env[as] = wi
			}
			appendRet(k, []uint32{ri, wi})

		case "stream.read", "stream.write":
			i := resolveHandle(env, op["end"])
			elemDesc, elemSz, align, numeric := asyncElemDesc(elemOf[i])
			async := opBool(op, "async", false)
			side, evCode := sideReadable, eventStreamRead
			if kind == "stream.write" {
				side, evCode = sideWritable, eventStreamWrite
			}
			def := streamCopyHostFunc(in, side, evCode, elemDesc, elemSz, align, numeric, async, mod, nil)
			stack := []uint64{uint64(i), uint64(opInt(op, "ptr")), uint64(opInt(op, "n"))}
			def.fn.Call(ctx, mod, stack)
			appendRet(k, []uint32{uint32(stack[0])})

		case "future.read", "future.write":
			i := resolveHandle(env, op["end"])
			elemDesc, elemSz, align, numeric := asyncElemDesc(elemOf[i])
			async := opBool(op, "async", false)
			side, evCode := sideReadable, eventFutureRead
			if kind == "future.write" {
				side, evCode = sideWritable, eventFutureWrite
			}
			def := futureCopyHostFunc(in, side, evCode, elemDesc, elemSz, align, numeric, async, mod, nil)
			stack := []uint64{uint64(i), uint64(opInt(op, "ptr"))}
			def.fn.Call(ctx, mod, stack)
			appendRet(k, []uint32{uint32(stack[0])})

		case "stream.cancel-read", "stream.cancel-write":
			i := resolveHandle(env, op["end"])
			elemDesc, _, _, _ := asyncElemDesc(elemOf[i])
			async := opBool(op, "async", false)
			side, evCode := sideReadable, eventStreamRead
			if kind == "stream.cancel-write" {
				side, evCode = sideWritable, eventStreamWrite
			}
			def := cancelCopyHostFunc(in, false, side, evCode, elemDesc, async)
			stack := []uint64{uint64(i)}
			def.fn.Call(ctx, mod, stack)
			appendRet(k, []uint32{uint32(stack[0])})

		case "future.cancel-read", "future.cancel-write":
			i := resolveHandle(env, op["end"])
			elemDesc, _, _, _ := asyncElemDesc(elemOf[i])
			async := opBool(op, "async", false)
			side, evCode := sideReadable, eventFutureRead
			if kind == "future.cancel-write" {
				side, evCode = sideWritable, eventFutureWrite
			}
			def := cancelCopyHostFunc(in, true, side, evCode, elemDesc, async)
			stack := []uint64{uint64(i)}
			def.fn.Call(ctx, mod, stack)
			appendRet(k, []uint32{uint32(stack[0])})

		case "stream.drop-readable", "stream.drop-writable":
			i := resolveHandle(env, op["end"])
			elemDesc, _, _, _ := asyncElemDesc(elemOf[i])
			side := sideReadable
			if kind == "stream.drop-writable" {
				side = sideWritable
			}
			def := streamDropHostFunc(in, side, elemDesc)
			def.fn.Call(ctx, mod, []uint64{uint64(i)})
			appendRet(k, nil)

		case "future.drop-readable", "future.drop-writable":
			i := resolveHandle(env, op["end"])
			elemDesc, _, _, _ := asyncElemDesc(elemOf[i])
			side := sideReadable
			if kind == "future.drop-writable" {
				side = sideWritable
			}
			def := futureDropHostFunc(in, side, elemDesc)
			def.fn.Call(ctx, mod, []uint64{uint64(i)})
			appendRet(k, nil)

		case "error-context.new":
			def := errorContextNewHostFunc(in, mod)
			stack := []uint64{uint64(opInt(op, "ptr")), uint64(opInt(op, "len"))}
			def.fn.Call(ctx, mod, stack)
			h := uint32(stack[0])
			bind(env, op, h)
			appendRet(k, []uint32{h})

		case "error-context.drop":
			i := resolveHandle(env, op["ec"])
			errorContextDropHostFunc(in).fn.Call(ctx, mod, []uint64{uint64(i)})
			appendRet(k, nil)

		case "mem.write":
			ptr := uint32(opInt(op, "ptr"))
			data, herr := hex.DecodeString(opStr(op, "bytes"))
			if herr != nil {
				t.Fatalf("%s: op %d: mem.write: bad hex: %v", sc.Name, k, herr)
			}
			if !mod.Memory().Write(ptr, data) {
				t.Fatalf("%s: op %d: mem.write: out of range", sc.Name, k)
			}

		case "mem.check":
			ptr := uint32(opInt(op, "ptr"))
			ln := uint32(opInt(op, "len"))
			b, ok := mod.Memory().Read(ptr, ln)
			if !ok {
				t.Fatalf("%s: op %d: mem.check: out of range", sc.Name, k)
			}
			trace = append(trace, asyncTraceEntry{Op: k, Kind: "mem", Bytes: hex.EncodeToString(b)})

		default:
			t.Fatalf("%s: op %d: unhandled op kind %q", sc.Name, k, kind)
		}
	}

	for k, op := range sc.Ops {
		currentOp = k
		trapped := func() (trapped bool) {
			defer func() {
				if r := recover(); r != nil {
					msg := fmt.Sprint(r)
					if err, ok := r.(error); ok {
						msg = err.Error()
					}
					entry := asyncTraceEntry{Op: k, Kind: "trap"}
					if strings.Contains(msg, "deadlock") {
						entry.Deadlock = true
					}
					trace = append(trace, entry)
					if sc.GoTrapContains != "" && !strings.Contains(msg, sc.GoTrapContains) {
						t.Errorf("%s: op %d: panic %q does not contain expected substring %q", sc.Name, k, msg, sc.GoTrapContains)
					}
					trapped = true
				}
			}()
			step(k, op)
			return false
		}()
		if trapped {
			return trace, nil
		}
	}

	return trace, snapshotAsyncTable(in)
}

// snapshotAsyncTable mirrors gen_async_oracle.py's snapshot_table: every
// live handle-table entry, sorted by index, with State/Copy populated only
// for the kinds that carry them (§2 footer).
func snapshotAsyncTable(in *Instance) []asyncTableEntry {
	out := in.resources.entriesSnapshot()
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}

// entriesSnapshot walks the handle table's live entries and classifies each
// (index, kind[, state/copy]) -- the table iteration test helper the design
// doc's Go harness skeleton names (§6).
func (t *handleTable) entriesSnapshot() []asyncTableEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]asyncTableEntry, 0, len(t.entries))
	for idx, e := range t.entries {
		entry := asyncTableEntry{Index: idx}
		switch v := e.(type) {
		case *waitableSet:
			entry.Kind = "waitable-set"
		case *subtask:
			entry.Kind = "subtask"
			entry.State = int(v.state)
		case *streamEnd:
			if v.side == sideReadable {
				entry.Kind = "stream-read"
			} else {
				entry.Kind = "stream-write"
			}
			entry.Copy = copyStateName(v.state)
		case *futureEnd:
			if v.side == sideReadable {
				entry.Kind = "future-read"
			} else {
				entry.Kind = "future-write"
			}
			entry.Copy = copyStateName(v.state)
		case *errorContext:
			entry.Kind = "error-context"
		default:
			panic(fmt.Sprintf("component/instance: async oracle: entriesSnapshot: unknown entry kind %T", e))
		}
		out = append(out, entry)
	}
	return out
}

// asyncElemDesc resolves a scenario's "elem" field ("u8"/"u32"/absent) to the
// (elemDesc, size, align, numeric) tuple streamCopyHostFunc/futureCopyHostFunc
// need, matching gen_async_oracle.py's elem_type + the reference's fixed u8/
// u32 sizes (size==align==1 or 4, both numeric -- design doc §1.3 restricts
// stream/future elements to u8/u32 for the oracle).
func asyncElemDesc(name string) (desc binary.TypeDesc, elemSz, align uint32, numeric bool) {
	switch name {
	case "":
		return nil, 0, 0, true
	case "u8":
		return binary.PrimitiveDesc{Prim: "u8"}, 1, 1, true
	case "u32":
		return binary.PrimitiveDesc{Prim: "u32"}, 4, 4, true
	default:
		panic(fmt.Sprintf("component/instance: async oracle: unsupported stream/future elem %q", name))
	}
}

func copyStateName(s copyState) string {
	switch s {
	case copyIdle:
		return "idle"
	case copyCopying:
		return "copying"
	case copyCancelling:
		return "cancelling"
	case copyDone:
		return "done"
	default:
		panic(fmt.Sprintf("component/instance: async oracle: unknown copyState %d", s))
	}
}
