;; Exports that return a stream and only then write to it: the write blocks
;; until the host reads, after the call has returned. With trap set, the task
;; traps once it resumes instead of finishing.
;;
;;   produce: a callback lift; its callback finishes the task.
;;   produce-stackful: the same with no callback, waiting on the write.
;;
;; The memory lives in an instance of its own, so the builtins that take it
;; exist before the module that calls them.
(component
  (type $st (stream u32))
  (core module $mh (memory (export "mem") 1))
  (core instance $mi (instantiate $mh))
  (alias core export $mi "mem" (core memory $mem))
  (core module $m
    (import "canon" "task.return" (func $tr (param i32)))
    (import "canon" "stream.new" (func $sn (result i64)))
    (import "canon" "stream.write" (func $sw (param i32 i32 i32) (result i32)))
    (import "canon" "stream.drop-writable" (func $sdw (param i32)))
    (import "canon" "waitable-set.new" (func $wsn (result i32)))
    (import "canon" "waitable.join" (func $wj (param i32 i32)))
    (import "canon" "waitable-set.wait" (func $wsw (param i32 i32) (result i32)))
    (import "mem" "mem" (memory 1))
    (global $w (mut i32) (i32.const 0))
    (global $trap (mut i32) (i32.const 0))
    (data (i32.const 0) "\07\00\00\00")
    ;; Returns the waitable set the blocked write is joined to.
    (func $start (param $trap i32) (result i32)
      (local $pair i64) (local $set i32)
      (global.set $trap (local.get $trap))
      (local.set $pair (call $sn))
      (global.set $w (i32.wrap_i64 (i64.shr_u (local.get $pair) (i64.const 32))))
      (call $tr (i32.wrap_i64 (local.get $pair)))
      (if (i32.ne (call $sw (global.get $w) (i32.const 0) (i32.const 1)) (i32.const -1))
        (then unreachable))
      (local.set $set (call $wsn))
      (call $wj (global.get $w) (local.get $set))
      (local.get $set))
    (func $finish
      (if (global.get $trap) (then unreachable))
      (call $wj (global.get $w) (i32.const 0))
      (call $sdw (global.get $w)))
    (func (export "run") (param $trap i32) (result i32)
      ;; WAIT (code 2) on the set
      (i32.or (i32.shl (call $start (local.get $trap)) (i32.const 4)) (i32.const 2)))
    (func (export "cb") (param i32 i32 i32) (result i32)
      (call $finish)
      (i32.const 0))
    (func (export "run-stackful") (param $trap i32)
      (drop (call $wsw (call $start (local.get $trap)) (i32.const 16)))
      (call $finish))
    (func (export "ra") (param i32 i32 i32 i32) (result i32) unreachable))
  (type $ft (func async (param "trap" bool) (result $st)))
  (canon task.return (result $st) (core func $tr))
  (canon stream.new $st (core func $sn))
  (canon stream.write $st async (memory $mem) (core func $sw))
  (canon stream.drop-writable $st (core func $sdw))
  (canon waitable-set.new (core func $wsn))
  (canon waitable.join (core func $wj))
  (canon waitable-set.wait (memory $mem) (core func $wsw))
  (core instance $i (instantiate $m
    (with "mem" (instance $mi))
    (with "canon" (instance
      (export "task.return" (func $tr))
      (export "stream.new" (func $sn))
      (export "stream.write" (func $sw))
      (export "stream.drop-writable" (func $sdw))
      (export "waitable-set.new" (func $wsn))
      (export "waitable.join" (func $wj))
      (export "waitable-set.wait" (func $wsw))))))
  (alias core export $i "run" (core func $run))
  (alias core export $i "run-stackful" (core func $run-stackful))
  (alias core export $i "cb" (core func $cb))
  (alias core export $i "ra" (core func $ra))
  (canon lift (core func $run) (memory $mem) (realloc $ra) async (callback $cb) (func $produce (type $ft)))
  (canon lift (core func $run-stackful) (memory $mem) (realloc $ra) async (func $produce-stackful (type $ft)))
  (export "produce" (func $produce))
  (export "produce-stackful" (func $produce-stackful))
)
