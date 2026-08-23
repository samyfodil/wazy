;; A shared memory with an i64 index type: the atomic instructions take their
;; address in that index type as well.
(module
  (memory (export "memory") i64 1 1 shared)

  (func (export "store") (param i64 i32) (i32.atomic.store (local.get 0) (local.get 1)))
  (func (export "load") (param i64) (result i32) (i32.atomic.load (local.get 0)))
  (func (export "rmw_add") (param i64 i32) (result i32)
    (i32.atomic.rmw.add (local.get 0) (local.get 1)))
  (func (export "cmpxchg") (param i64 i32 i32) (result i32)
    (i32.atomic.rmw.cmpxchg (local.get 0) (local.get 1) (local.get 2)))
  (func (export "store64") (param i64 i64) (i64.atomic.store (local.get 0) (local.get 1)))
  (func (export "load64") (param i64) (result i64) (i64.atomic.load (local.get 0)))
  (func (export "notify") (param i64) (result i32) (memory.atomic.notify (local.get 0) (i32.const 1)))
  (func (export "wait32") (param i64 i32) (result i32)
    (memory.atomic.wait32 (local.get 0) (local.get 1) (i64.const 0)))
)
