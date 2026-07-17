;; Phase 2 acceptance fixture (b): a guest that READS a future<u32> the host
;; WRITES via a host FutureWriter (docs/component-model-async-phase2-design.md
;; §3.4's mirror trace: "Host feeds a guest export a stream", specialized to a
;; future). The host arranges the FutureWriter.Set BEFORE Call, so the
;; guest's future.read rendezvouses synchronously; no callback/async lift
;; needed here either.
(component
  (type $ft (future u32))

  (core module $m
    (import "canon" "future_read" (func $future_read (param i32 i32) (result i32)))
    (memory (export "mem") 1)
    (func (export "run") (param $h i32) (result i32)
      (drop (call $future_read (local.get $h) (i32.const 0)))
      (i32.load (i32.const 0))))

  (canon future.read $ft (core func $future_read))
  (core instance $i (instantiate $m (with "canon" (instance
    (export "future_read" (func $future_read))))))

  (alias core export $i "run" (core func $run))
  (alias core export $i "mem" (core memory $mem))
  (type $ft2 (func (param "f" $ft) (result u32)))
  (canon lift (core func $run) (memory $mem) (func $lifted (type $ft2)))
  (export "process" (func $lifted))
)
