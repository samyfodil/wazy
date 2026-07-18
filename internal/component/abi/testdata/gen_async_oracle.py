#!/usr/bin/env python3
"""Generates testdata/async_oracle_golden.json from testdata/async_scenarios.json
by interpreting each scenario's op sequence against the vendored reference
implementation (testdata/definitions.py), through a REAL async lift
(Store.invoke/store.lift with ft.async_=True, opts.async_=True,
opts.callback=None) so current_thread()/current_task() are real and blocking
builtins genuinely block the scripted callee's own green thread.

This is the reference (Python) half of the async differential oracle:
async_oracle_test.go (package instance) interprets the SAME scenario battery
against wazy's real async builtins/runtime and diffs the resulting trace
against this golden file byte-for-byte (mod the documented exclusions in
docs/component-model-async-oracle-design.md §5).

Usage:
    python3 testdata/gen_async_oracle.py

Run from the internal/component/abi directory (or anywhere; paths below are
resolved relative to this script's location).

See docs/component-model-async-oracle-design.md for the full design this
implements: the scenario/golden schemas (§1/§2), the determinism pins (§3),
and the honest scope this oracle does and doesn't cover (§5).
"""
import hashlib
import json
import os
import sys
import threading

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from gen_oracle import load_definitions  # noqa: E402 (reuse the existing loader verbatim)

ref = load_definitions()

# ---- §3 determinism pins ----
# 1: kill wait_until's random-early-return coin flip -- a thread whose
#    predicate is already true still enqueues + blocks, exactly one behavior.
ref.DETERMINISTIC_PROFILE = True
# 2: WaitableSet.get_pending_event scans elems in JOIN ORDER (no shuffle) --
#    matches wazy's waitableSet.getPendingEvent (first joined member with a
#    pending event).
ref.random.shuffle = lambda l: None
# 3: every remaining random.choice site chooses among READY THREADS, and
#    every oracle scenario has exactly ONE guest thread -- so every candidate
#    set is a singleton or empty. Defensive: assert the invariant instead of
#    silently letting real randomness in (list(set) iteration order is
#    genuinely nondeterministic across CPython runs).
def _singleton_choice(l):
    l = list(l)
    if len(l) != 1:
        raise AssertionError(f"oracle invariant: non-singleton choice ({len(l)} candidates)")
    return l[0]


ref.random.choice = _singleton_choice

# Threading landmine: definitions.py's Thread/Continuation machinery (~258-320)
# is built on REAL OS threads (threading.Thread) with lock handoff, not
# generators/coroutines. A deliberately-deadlocking scenario (the oracle's own
# normalized-deadlock trap, §2.3) leaves its scripted callee's thread parked
# forever inside block() -- by design, our driver just stops pumping it. The
# vendored cont_new (~276) spawns that thread as non-daemon, so at process
# exit CPython would block waiting for it to finish, hanging this script
# forever. Force every thread cont_new spawns to be daemon so an abandoned
# parked scenario thread is killed at interpreter exit instead. This is a
# harness-level monkeypatch (same category as the shuffle/choice patches
# above), not an edit to definitions.py itself.
_OrigThread = ref.threading.Thread


class _DaemonThread(_OrigThread):
    def __init__(self, *a, **kw):
        super().__init__(*a, **kw)
        self.daemon = True


ref.threading.Thread = _DaemonThread

# ---- harness scratch-memory layout (must match async_oracle_test.go's
# eventPtr/importRetptrBase/... constants exactly -- both interpreters read
# these ops' addresses from the SAME scenario JSON, but the WAIT/POLL event
# ptr and the import retptr base are harness conventions, not scenario
# fields, so they're pinned here as constants both sides hardcode). ----
EVENT_PTR = 0x100                 # waitable-set.wait/poll's (p1,p2) out-struct
IMPORT_RETPTR_BASE = 0x200        # import.call's u32 result out-pointer
IMPORT_RETPTR_STRIDE = 16

MEM_SIZE = 65536


def bump_realloc(mem):
    """A trivial bump allocator for the reference's CanonicalOptions.realloc:
    old_ptr==0 (fresh alloc) grows a monotonic high-water mark; anything else
    (resize/free) is unsupported (no scenario in this battery needs it -- the
    only realloc-backed store is error-context.debug-message's string, a
    single new allocation per call). NOT comparable byte-for-byte against
    wazy's real cabi_realloc address (see async_oracle_test.go's matching
    note); scenarios needing an allocated address only compare the allocated
    BYTES, never the pointer value itself.
    """
    state = {"next": 0x8000}  # start high, away from the harness's own fixed scratch addresses

    def realloc(old_ptr, old_size, align, new_size):
        if old_ptr != 0 or old_size != 0:
            raise NotImplementedError("bump_realloc: only fresh allocations are supported by this harness")
        if new_size == 0:
            return 0
        ptr = (state["next"] + align - 1) & ~(align - 1)
        if ptr + new_size > len(mem):
            raise MemoryError("bump_realloc: out of scratch memory")
        state["next"] = ptr + new_size
        return ptr

    return realloc


PRIM_TYPES = {
    "u8": ref.U8Type, "u16": ref.U16Type, "u32": ref.U32Type, "u64": ref.U64Type,
    "s8": ref.S8Type, "s16": ref.S16Type, "s32": ref.S32Type, "s64": ref.S64Type,
}


def elem_type(name):
    return None if name is None else PRIM_TYPES[name]()


class ScenarioError(Exception):
    """A genuine harness bug (not a scenario-level ref.Trap) -- stashed while
    the scripted callee's OS thread is still alive and re-raised from the
    main thread, never across the thread boundary (see the module docstring's
    threading note)."""


def resolve_handle(env, raw):
    """{"$": "name"} -> env[name]; a bare int -> itself (deliberately raw,
    for stale/wrong-index trap scenarios); 0 for waitable.join's "leave set"
    arm."""
    if isinstance(raw, dict):
        return env[raw["$"]]
    return raw


def kind_name(entry):
    if isinstance(entry, ref.WaitableSet):
        return "waitable-set"
    if isinstance(entry, ref.Subtask):
        return "subtask"
    if isinstance(entry, ref.ReadableStreamEnd):
        return "stream-read"
    if isinstance(entry, ref.WritableStreamEnd):
        return "stream-write"
    if isinstance(entry, ref.ReadableFutureEnd):
        return "future-read"
    if isinstance(entry, ref.WritableFutureEnd):
        return "future-write"
    if isinstance(entry, ref.ErrorContext):
        return "error-context"
    raise ScenarioError(f"unknown table entry kind: {type(entry)}")


_COPY_STATE_NAME = {
    ref.CopyState.IDLE: "idle",
    ref.CopyState.COPYING: "copying",
    ref.CopyState.CANCELLING_COPY: "cancelling",
    ref.CopyState.DONE: "done",
}


def snapshot_table(inst):
    out = []
    for i, e in enumerate(inst.handles.array):
        if e is None:
            continue
        entry = {"index": i, "kind": kind_name(e)}
        if isinstance(e, ref.Subtask):
            entry["state"] = int(e.state)
        elif isinstance(e, ref.CopyEnd):
            entry["copy"] = _COPY_STATE_NAME[e.state]
        out.append(entry)
    out.sort(key=lambda x: x["index"])
    return out


def make_ft(task_result):
    result_types = [] if task_result is None else [PRIM_TYPES[task_result]()]

    class FT:
        async_ = True

        def param_types(self):
            return []

        def result_type(self):
            return result_types

    ft = FT()
    ft.result = None if task_result is None else PRIM_TYPES[task_result]()
    return ft


def run_scenario(sc):
    trace = []
    env = {}
    elem_of = {}  # stream/future handle (either end) -> its "elem" scenario field (None for bare)
    current_op = [-1]
    harness_error = [None]
    done = [False]
    trapped = [False]  # an in-scenario ref.Trap fired (as opposed to normal completion or deadlock)
    deferred = []  # [(after, registration_index, fire_fn)], popped smallest-after first
    reg_counter = [0]
    import_ordinal = [0]

    store = ref.Store()
    inst = ref.ComponentInstance(store)
    mem = ref.MemInst(bytearray(MEM_SIZE), 'i32')
    realloc = bump_realloc(mem)
    opts = ref.CanonicalOptions(memory=mem, realloc=realloc, async_=True, callback=None)
    ft = make_ft(sc["task_result"])

    def register_deferred(after, fire_fn):
        reg_counter[0] += 1
        deferred.append((after, reg_counter[0], fire_fn))

    def bind(op, value):
        if "as" in op:
            env[op["as"]] = value

    def force_resolved():
        # Directly poke Task.State.RESOLVED (bypassing return_/cancel's
        # on_resolve) so Task.exit_implicit_thread's own trap_if(state !=
        # RESOLVED) (~522) can't double-fault after we've already recorded
        # this scenario's trap entry -- see the module docstring.
        current_task = ref.current_task()
        current_task.state = ref.Task.State.RESOLVED

    def cx_of(o):
        return ref.LiftLowerContext(o, inst)

    def mem_write(ptr, data):
        mem[ptr:ptr + len(data)] = data

    def make_import_callee(behavior, resolve_after, result):
        def callee(on_start, on_resolve, caller):
            on_start()
            resolved = [False]

            def do_resolve(vals):
                if resolved[0]:
                    return
                resolved[0] = True
                on_resolve(vals)

            def on_cancel():
                if behavior == "cancel-resolves-cancelled":
                    do_resolve(None)
                elif behavior == "cancel-completes":
                    vals = [] if result is None else [result]
                    do_resolve(vals)
                # "resolve" / "never" / "ignore-cancel": no synchronous effect;
                # ignore-cancel still resolves at its scheduled round below.

            if behavior in ("cancel-resolves-cancelled", "cancel-completes"):
                return on_cancel  # resolves ONLY via cancellation
            if behavior == "never":
                return on_cancel  # never resolves
            vals = [] if result is None else [result]
            if resolve_after == 0:
                do_resolve(vals)
            else:
                register_deferred(resolve_after, lambda: do_resolve(vals))
            return on_cancel
        return callee

    def step(k, op):
        kind = op["op"]

        if kind == "waitable-set.new":
            i = ref.canon_waitable_set_new()[0]
            bind(op, i)
            trace.append({"op": k, "kind": "ret", "vals": [i]})

        elif kind == "waitable-set.wait":
            si = resolve_handle(env, op["set"])
            code = ref.canon_waitable_set_wait(op.get("cancellable", False), mem, si, EVENT_PTR)[0]
            p1, p2 = mem[EVENT_PTR:EVENT_PTR + 4], mem[EVENT_PTR + 4:EVENT_PTR + 8]
            trace.append({"op": k, "kind": "event", "code": int(code),
                           "p1": int.from_bytes(p1, "little"), "p2": int.from_bytes(p2, "little")})

        elif kind == "waitable-set.poll":
            si = resolve_handle(env, op["set"])
            code = ref.canon_waitable_set_poll(op.get("cancellable", False), mem, si, EVENT_PTR)[0]
            p1, p2 = mem[EVENT_PTR:EVENT_PTR + 4], mem[EVENT_PTR + 4:EVENT_PTR + 8]
            trace.append({"op": k, "kind": "event", "code": int(code),
                           "p1": int.from_bytes(p1, "little"), "p2": int.from_bytes(p2, "little")})

        elif kind == "waitable-set.drop":
            si = resolve_handle(env, op["set"])
            ref.canon_waitable_set_drop(si)
            trace.append({"op": k, "kind": "ret", "vals": []})

        elif kind == "waitable.join":
            wi = resolve_handle(env, op["w"])
            si = resolve_handle(env, op["set"])
            ref.canon_waitable_join(wi, si)
            trace.append({"op": k, "kind": "ret", "vals": []})

        elif kind == "import.call":
            result = op.get("result")
            callee = make_import_callee(op.get("behavior", "resolve"), op.get("resolve_after", 0), result)
            ft_i = make_ft("u32" if result is not None else None)
            # An async-lowered import's core signature is zero declared
            # params (§1.3: imports take no params) plus, when the import
            # declares a result, ONE trailing out-pointer param -- flat_args
            # is that raw core arg list. canon_lower's on_start consumes
            # nothing from it (ft_i.param_types() == []), so on_resolve's
            # lower_flat_values(..., out_param=flat_args) pulls the retptr
            # from position 0 via out_param.next() -- the real async-lower
            # result-spill path (definitions.py ~2118-2138), not a shortcut.
            flat_args = []
            if result is not None:
                retptr = IMPORT_RETPTR_BASE + IMPORT_RETPTR_STRIDE * import_ordinal[0]
                import_ordinal[0] += 1
                flat_args = [retptr]
            packed = ref.canon_lower(callee, ft_i, opts, flat_args)[0]
            i = int(packed)
            state = i & 0xF
            if state != ref.Subtask.State.RETURNED:
                bind(op, i >> 4)
            trace.append({"op": k, "kind": "ret", "vals": [i]})

        elif kind == "subtask.cancel":
            i = resolve_handle(env, op["sub"])
            packed = ref.canon_subtask_cancel(op.get("async", False), i)[0]
            trace.append({"op": k, "kind": "ret", "vals": [int(packed)]})

        elif kind == "subtask.drop":
            i = resolve_handle(env, op["sub"])
            ref.canon_subtask_drop(i)
            trace.append({"op": k, "kind": "ret", "vals": []})

        elif kind == "task.return":
            vals = op.get("vals", [])
            result_type = None if sc["task_result"] is None else PRIM_TYPES[sc["task_result"]]()
            ref.canon_task_return(result_type, opts, vals)
            trace.append({"op": k, "kind": "ret", "vals": []})

        elif kind == "task.cancel":
            ref.canon_task_cancel()
            trace.append({"op": k, "kind": "ret", "vals": []})

        elif kind == "host.cancel-root":
            register_deferred(op["after"], lambda: on_cancel_root())
            trace.append({"op": k, "kind": "ret", "vals": []})

        elif kind == "context.get":
            v = ref.canon_context_get("i32", op["slot"])[0]
            trace.append({"op": k, "kind": "ret", "vals": [int(v)]})

        elif kind == "context.set":
            ref.canon_context_set("i32", op["slot"], op["val"])
            trace.append({"op": k, "kind": "ret", "vals": []})

        elif kind == "backpressure.inc":
            ref.canon_backpressure_inc()
            trace.append({"op": k, "kind": "ret", "vals": []})

        elif kind == "backpressure.dec":
            ref.canon_backpressure_dec()
            trace.append({"op": k, "kind": "ret", "vals": []})

        elif kind == "stream.new":
            elem_name = op.get("elem")
            st = ref.StreamType(elem_type(elem_name))
            packed = ref.canon_stream_new(st)[0]
            ri, wi = packed & 0xFFFFFFFF, (packed >> 32) & 0xFFFFFFFF
            elem_of[ri] = elem_of[wi] = elem_name
            if "as_read" in op:
                env[op["as_read"]] = ri
            if "as_write" in op:
                env[op["as_write"]] = wi
            trace.append({"op": k, "kind": "ret", "vals": [ri, wi]})

        elif kind == "future.new":
            elem_name = op.get("elem")
            ft2 = ref.FutureType(elem_type(elem_name))
            packed = ref.canon_future_new(ft2)[0]
            ri, wi = packed & 0xFFFFFFFF, (packed >> 32) & 0xFFFFFFFF
            elem_of[ri] = elem_of[wi] = elem_name
            if "as_read" in op:
                env[op["as_read"]] = ri
            if "as_write" in op:
                env[op["as_write"]] = wi
            trace.append({"op": k, "kind": "ret", "vals": [ri, wi]})

        elif kind in ("stream.read", "stream.write"):
            i = resolve_handle(env, op["end"])
            copy_opts = ref.CanonicalOptions(memory=mem, realloc=realloc, async_=op.get("async", False))
            st = ref.StreamType(elem_type(elem_of.get(i)))
            fn = ref.canon_stream_read if kind == "stream.read" else ref.canon_stream_write
            payload = fn(st, copy_opts, i, op["ptr"], op["n"])[0]
            trace.append({"op": k, "kind": "ret", "vals": [int(payload) & 0xFFFFFFFF]})

        elif kind in ("future.read", "future.write"):
            i = resolve_handle(env, op["end"])
            copy_opts = ref.CanonicalOptions(memory=mem, realloc=realloc, async_=op.get("async", False))
            ft2 = ref.FutureType(elem_type(elem_of.get(i)))
            fn = ref.canon_future_read if kind == "future.read" else ref.canon_future_write
            payload = fn(ft2, copy_opts, i, op["ptr"])[0]
            trace.append({"op": k, "kind": "ret", "vals": [int(payload) & 0xFFFFFFFF]})

        elif kind in ("stream.cancel-read", "stream.cancel-write"):
            i = resolve_handle(env, op["end"])
            st = ref.StreamType(elem_type(elem_of.get(i)))
            fn = ref.canon_stream_cancel_read if kind == "stream.cancel-read" else ref.canon_stream_cancel_write
            payload = fn(st, op.get("async", False), i)[0]
            trace.append({"op": k, "kind": "ret", "vals": [int(payload) & 0xFFFFFFFF]})

        elif kind in ("future.cancel-read", "future.cancel-write"):
            i = resolve_handle(env, op["end"])
            ft2 = ref.FutureType(elem_type(elem_of.get(i)))
            fn = ref.canon_future_cancel_read if kind == "future.cancel-read" else ref.canon_future_cancel_write
            payload = fn(ft2, op.get("async", False), i)[0]
            trace.append({"op": k, "kind": "ret", "vals": [int(payload) & 0xFFFFFFFF]})

        elif kind in ("stream.drop-readable", "stream.drop-writable"):
            i = resolve_handle(env, op["end"])
            st = ref.StreamType(elem_type(elem_of.get(i)))
            if kind == "stream.drop-readable":
                ref.canon_stream_drop_readable(st, i)
            else:
                ref.canon_stream_drop_writable(st, i)
            trace.append({"op": k, "kind": "ret", "vals": []})

        elif kind in ("future.drop-readable", "future.drop-writable"):
            i = resolve_handle(env, op["end"])
            ft2 = ref.FutureType(elem_type(elem_of.get(i)))
            if kind == "future.drop-readable":
                ref.canon_future_drop_readable(ft2, i)
            else:
                ref.canon_future_drop_writable(ft2, i)
            trace.append({"op": k, "kind": "ret", "vals": []})

        elif kind == "error-context.new":
            ec_opts = ref.CanonicalOptions(memory=mem, realloc=realloc, async_=False)
            i = ref.canon_error_context_new(ec_opts, op["ptr"], op["len"])[0]
            bind(op, i)
            trace.append({"op": k, "kind": "ret", "vals": [int(i)]})

        elif kind == "error-context.drop":
            i = resolve_handle(env, op["ec"])
            ref.canon_error_context_drop(i)
            trace.append({"op": k, "kind": "ret", "vals": []})

        elif kind == "mem.write":
            mem_write(op["ptr"], bytes.fromhex(op["bytes"]))

        elif kind == "mem.check":
            data = bytes(mem[op["ptr"]:op["ptr"] + op["len"]])
            trace.append({"op": k, "kind": "mem", "bytes": data.hex()})

        else:
            raise ScenarioError(f"unhandled op kind: {kind!r}")

    def on_cancel_root():
        on_cancel()

    def top_callee(flat_args):
        try:
            for k, op in enumerate(sc["ops"]):
                current_op[0] = k
                try:
                    step(k, op)
                except ref.Trap:
                    entry = {"op": k, "kind": "trap"}
                    trace.append(entry)
                    force_resolved()
                    trapped[0] = True
                    return []
            return []
        except ScenarioError:
            raise
        except Exception as e:  # noqa: BLE001 -- see module docstring's threading note
            harness_error[0] = e
            force_resolved()
            return []
        finally:
            done[0] = True

    def on_task_resolve(r):
        # Task.return_/Task.cancel call on_resolve synchronously (~556-567),
        # so current_op[0] is still the op index of the task.return/
        # task.cancel call that triggered this -- matches the golden
        # schema's task-resolve entries sharing that op's index (§2).
        cancelled = r is None
        vals = [] if cancelled else [int(v) for v in r]
        trace.append({"op": current_op[0], "kind": "task-resolve", "cancelled": cancelled, "vals": vals})

    fi = store.lift(top_callee, ft, opts, inst)
    on_cancel = store.invoke(fi, on_start=lambda: [], on_resolve=on_task_resolve)

    rounds = 0
    while not done[0]:
        rounds += 1
        if rounds > 100000:
            raise ScenarioError(f"scenario {sc['name']!r}: driver did not converge (bug)")
        fired = False
        if deferred:
            deferred.sort(key=lambda a: (a[0], a[1]))
            _, _, fn = deferred.pop(0)
            fn()
            fired = True
        ready = any(th.ready() for th in store.waiting)
        if not fired and not ready:
            trace.append({"op": current_op[0], "kind": "trap", "deadlock": True})
            return {"trace": trace, "table": None}
        store.tick()

    if harness_error[0] is not None:
        raise harness_error[0]

    # table is null after ANY trap (§2 golden schema): "post-trap table state
    # is not part of the reference's observable contract; wazy's trap tears
    # down the instance" -- not just the deadlock arm above, which already
    # returns early with table:None.
    if trapped[0]:
        return {"trace": trace, "table": None}
    return {"trace": trace, "table": snapshot_table(inst)}


def scenarios_sha256(raw_bytes):
    return hashlib.sha256(raw_bytes).hexdigest()


def main():
    scenarios_path = os.path.join(HERE, "async_scenarios.json")
    golden_path = os.path.join(HERE, "async_oracle_golden.json")

    with open(scenarios_path, "rb") as f:
        raw = f.read()
    battery = json.loads(raw)["scenarios"]

    golden = {"scenarios_sha256": scenarios_sha256(raw), "scenarios": {}}
    for sc in battery:
        name = sc["name"]
        try:
            golden["scenarios"][name] = run_scenario(sc)
        except Exception as e:
            print(f"scenario {name!r} failed: {e!r}", file=sys.stderr)
            raise

    with open(golden_path, "w") as f:
        json.dump(golden, f, indent=2, sort_keys=True)
        f.write("\n")

    print(f"wrote {len(golden['scenarios'])} golden scenario trace(s) to {golden_path}")


if __name__ == "__main__":
    main()
