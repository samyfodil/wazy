;; Atomic accesses to a 64-bit memory whose effective address can pass 2^64: see atomic_trap_order_test.go.
(module
  (memory i64 1 1 shared)
  (func (export "load") (param $p i64) (result i32) (i32.atomic.load offset=6 (local.get $p)))
  (func (export "rmw") (param $p i64) (result i64) (i64.atomic.rmw.add offset=3 (local.get $p) (i64.const 1)))
)
