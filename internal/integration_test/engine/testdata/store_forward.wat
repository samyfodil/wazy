;; Loads of what a store just wrote: see store_forward_test.go.
(module
  (memory 1)
  (func $poke (param i32) (i32.store (local.get 0) (i32.const 77)))
  ;; Read back: the stored value.
  (func (export "same") (param $p i32) (param $v i32) (result i32)
    (i32.store offset=4 (local.get $p) (local.get $v))
    (i32.load offset=4 (local.get $p)))
  ;; A second store, through another address value that may be the same address.
  (func (export "alias") (param $p i32) (param $q i32) (result i32)
    (i32.store (local.get $p) (i32.const 1))
    (i32.store (local.get $q) (i32.const 2))
    (i32.load (local.get $p)))
  ;; Narrower, and reinterpreted.
  (func (export "narrow") (param $p i32) (result i32)
    (i64.store (local.get $p) (i64.const 0x1122334455667788))
    (i32.add (i32.load8_u (local.get $p)) (i32.wrap_i64 (i64.reinterpret_f64 (f64.load (local.get $p))))))
  ;; A call and a fill between.
  (func (export "call") (param $p i32) (result i32)
    (i32.store (local.get $p) (i32.const 5))
    (call $poke (local.get $p))
    (i32.load (local.get $p)))
  (func (export "fill") (param $p i32) (result i32)
    (i32.store (local.get $p) (i32.const 5))
    (memory.fill (local.get $p) (i32.const 9) (i32.const 4))
    (i32.load (local.get $p)))
  ;; The load in the next block.
  (func (export "block") (param $p i32) (param $c i32) (result i32)
    (i32.store (local.get $p) (i32.const 3))
    (if (local.get $c) (then (i32.store (local.get $p) (i32.const 4))))
    (i32.load (local.get $p)))
  ;; NaN bits survive.
  (func (export "nan") (param $p i32) (result i32)
    (f32.store (local.get $p) (f32.reinterpret_i32 (i32.const 0x7fa00001)))
    (i32.reinterpret_f32 (f32.load (local.get $p))))
  ;; Out of bounds: the store traps first.
  (func (export "oob") (param $p i32) (result i64)
    (i64.store (local.get $p) (i64.const 1))
    (i64.load (local.get $p)))
)
