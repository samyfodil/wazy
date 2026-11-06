;; Atomic accesses both misaligned and out of bounds: see atomic_trap_order_test.go.
(module
  (memory 1 1 shared)
  (func (export "load") (param $p i32) (result i32) (i32.atomic.load offset=2 (local.get $p)))
  (func (export "load64") (param $p i32) (result i64) (i64.atomic.load (local.get $p)))
  (func (export "rmw") (param $p i32) (result i32) (i32.atomic.rmw.add (local.get $p) (i32.const 1)))
  (func (export "store16") (param $p i32) (i32.atomic.store16 (local.get $p) (i32.const 1)))
  (func (export "notify") (param $p i32) (result i32) (memory.atomic.notify (local.get $p) (i32.const 1)))
  (func (export "wait") (param $p i32) (result i32) (memory.atomic.wait32 (local.get $p) (i32.const 1) (i64.const 0)))
)
