(component
  (core module $memmod
    (memory (export "mem") 1))
  (core instance $mi (instantiate $memmod))
  (alias core export $mi "mem" (core memory $mem))

  (type $ft (func async (result u32)))
  (import "get" (func $get_import (type $ft)))
  (canon lower (func $get_import) async (memory $mem) (core func $lowered_get))

  (core module $m
    (import "canon" "task.return" (func $tr (param i32)))
    (import "canon" "task.cancel" (func $tc))
    (import "canon" "wsnew" (func $wsnew (result i32)))
    (import "canon" "wjoin" (func $wjoin (param i32 i32)))
    (import "canon" "get" (func $get (param i32) (result i32)))
    (import "canon" "mem" (memory 1))
    (global $wset (mut i32) (i32.const 0))
    (global $subtaski (mut i32) (i32.const 0))
    (func (export "run") (result i32)
      (local $packed i32)
      (local.set $packed (call $get (i32.const 0)))
      (global.set $subtaski (i32.shr_u (local.get $packed) (i32.const 4)))
      (global.set $wset (call $wsnew))
      (call $wjoin (global.get $subtaski) (global.get $wset))
      (i32.or (i32.const 2) (i32.shl (global.get $wset) (i32.const 4))))
    (func (export "cb") (param $event_code i32) (param $p1 i32) (param $p2 i32) (result i32)
      (if (i32.eq (local.get $event_code) (i32.const 6)) ;; TASK_CANCELLED
        (then
          (call $tc)
          (return (i32.const 0)))) ;; EXIT
      (call $tr (i32.load (i32.const 0)))
      (i32.const 0)) ;; EXIT
    (func (export "ra") (param i32 i32 i32 i32) (result i32) unreachable))

  (canon task.return (result u32) (core func $tr))
  (canon task.cancel (core func $tc))
  (canon waitable-set.new (core func $wsnew))
  (canon waitable.join (core func $wjoin))

  (core instance $i (instantiate $m (with "canon" (instance
    (export "task.return" (func $tr))
    (export "task.cancel" (func $tc))
    (export "wsnew" (func $wsnew))
    (export "wjoin" (func $wjoin))
    (export "get" (func $lowered_get))
    (export "mem" (memory $mem))))))

  (alias core export $i "run" (core func $run))
  (alias core export $i "cb" (core func $cb))
  (alias core export $i "ra" (core func $ra))
  (canon lift (core func $run) (memory $mem) (realloc $ra) async (callback $cb) (func $lifted (type $ft)))
  (export "run-async" (func $lifted))
)
