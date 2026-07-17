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
    (import "canon" "get" (func $get (param i32) (result i32)))
    (import "canon" "mem" (memory 1))
    (func (export "run") (result i32)
      (local $packed i32)
      (local.set $packed (call $get (i32.const 0)))
      (call $tr (i32.load (i32.const 0)))
      (i32.const 0)) ;; EXIT (packed is RETURNED; no WAIT ever needed)
    (func (export "cb") (param i32 i32 i32) (result i32)
      (i32.const 0))
    (func (export "ra") (param i32 i32 i32 i32) (result i32) unreachable))

  (canon task.return (result u32) (core func $tr))

  (core instance $i (instantiate $m (with "canon" (instance
    (export "task.return" (func $tr))
    (export "get" (func $lowered_get))
    (export "mem" (memory $mem))))))

  (alias core export $i "run" (core func $run))
  (alias core export $i "cb" (core func $cb))
  (alias core export $i "ra" (core func $ra))
  (canon lift (core func $run) (memory $mem) (realloc $ra) async (callback $cb) (func $lifted (type $ft)))
  (export "run-async" (func $lifted))
)
