(module
  (type $s (struct (field (mut i32))))
  (memory 1 1 shared)

  ;; Counts n down through return_call alone: no loop, so no loop header to park at.
  (func $spin (export "spin") (param $n i64) (result i64)
    (if (result i64) (i64.eqz (local.get $n))
      (then (i64.const 0))
      (else (return_call $spin (i64.sub (local.get $n) (i64.const 1))))))

  ;; Counts n down in a loop that does nothing else: the kind of call a collection waits longest for.
  (func (export "count") (param $n i64)
    (loop $l
      (br_if $l (i64.ne (local.tee $n (i64.sub (local.get $n) (i64.const 1))) (i64.const 0)))))

  ;; Allocates n structs and drops them, enough to ask for a collection.
  (func $churn (export "churn") (param $n i32)
    (local $i i32)
    (loop $l
      (drop (struct.new $s (local.get $i)))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $i) (local.get $n)))))

  ;; Blocks until notified, or for 20 seconds.
  (func (export "wait") (result i32)
    (memory.atomic.wait32 (i32.const 0) (i32.const 0) (i64.const 20000000000)))

  ;; Churns, then wakes the waiter.
  (func (export "churn_then_wake") (param $n i32) (result i32)
    (call $churn (local.get $n))
    (memory.atomic.notify (i32.const 0) (i32.const 1)))
)
