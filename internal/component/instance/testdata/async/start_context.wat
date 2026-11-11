;; A core start function reading and writing context storage: it runs in a
;; synchronous task of its own (Concurrency.md, "Interaction with the start
;; function"), as wasi-sdk's WASI 0.3 libc relies on, keeping the stack
;; pointer in context slot 0. f returns what the start function read back
;; (42), and g reads the slot in its own call's task (0).
(component
  (core func $get (canon context.get i32 0))
  (core func $set (canon context.set i32 0))
  (core module $m
    (import "" "get" (func $get (result i32)))
    (import "" "set" (func $set (param i32)))
    (global $seen (mut i32) (i32.const -1))
    (func $start
      (call $set (i32.const 42))
      (global.set $seen (call $get)))
    (start $start)
    (func (export "f") (result i32) (global.get $seen))
    (func (export "g") (result i32) (call $get)))
  (core instance $i (instantiate $m
    (with "" (instance (export "get" (func $get)) (export "set" (func $set))))))
  (func (export "f") (result u32) (canon lift (core func $i "f")))
  (func (export "g") (result u32) (canon lift (core func $i "g")))
)
