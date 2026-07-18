;; A minimal Component Model component that spawns a thread.
;;
;; Build: wasm-tools parse thread.wat -o thread.wasm
;;
;; The lifted `run-async` export is a stackful async function. Its core `run`
;; spawns a worker thread with `thread.new-indirect` (created SUSPENDED) and
;; hands control to it with `thread.yield-then-resume`. The worker runs on that
;; thread and calls `task.return(99)`, resolving the task.
(component
  ;; Standalone table+memory instance so the canon builtins can reference the
  ;; func table (the trick the conformance fixtures use).
  (core module $MemTable
    (table (export "ftbl") 1 funcref)
    (memory (export "mem") 1))
  (core instance $memTable (instantiate $MemTable))

  (core module $m
    (import "" "task.return" (func $task.return (param i32)))
    (import "" "thread.new-indirect" (func $thread.new-indirect (param i32 i32) (result i32)))
    (import "" "thread.yield-then-resume" (func $thread.yield-then-resume (param i32) (result i32)))
    (import "" "ftbl" (table $ftbl 1 funcref))

    ;; Worker: runs on a spawned thread, returns its argument to the task.
    (func $worker (param $arg i32)
      (call $task.return (local.get $arg)))
    (elem (table $ftbl) (i32.const 0) func $worker)

    ;; run (stackful async, no callback): spawn worker(99) as a SUSPENDED
    ;; thread, then yield-then-resume into it. The worker calls task.return(99);
    ;; control comes back here and run returns, resolving the task.
    (func (export "run")
      (drop (call $thread.yield-then-resume
              (call $thread.new-indirect (i32.const 0) (i32.const 99)))))

    (func (export "ra") (param i32 i32 i32 i32) (result i32) unreachable))

  (core type $start-func-ty (func (param i32)))
  (alias core export $memTable "ftbl" (core table $ftbl))
  (alias core export $memTable "mem" (core memory $mem))

  (canon task.return (result u32) (core func $task.return))
  (core func $thread.new-indirect (canon thread.new-indirect $start-func-ty (table $ftbl)))
  (core func $thread.yield-then-resume (canon thread.yield-then-resume))

  (core instance $i (instantiate $m
    (with "" (instance
      (export "task.return" (func $task.return))
      (export "thread.new-indirect" (func $thread.new-indirect))
      (export "thread.yield-then-resume" (func $thread.yield-then-resume))
      (export "ftbl" (table $ftbl))))))

  (alias core export $i "run" (core func $run))
  (alias core export $i "ra" (core func $ra))

  (type $ft (func async (result u32)))
  (canon lift (core func $run) (memory $mem) (realloc $ra) async (func $lifted (type $ft)))
  (export "run-async" (func $lifted))
)
