;; Has no safepoint of its own, so on its own it would need no root region,
;; but runs other modules' functions on its stack: reserve_callee.wat's
;; through an import, and whatever $prepare leaves in its table through
;; call_indirect. See TestRootRegionIsTheEngines.
(module
  (import "env" "prepare" (func $prepare))
  (import "callee" "f" (func $f (param i32) (result i64)))
  (table (export "t") 1 funcref)
  (type $v (func (param i32) (result i64)))
  (func (export "direct") (param i32) (result i64)
    (call $f (local.get 0)))
  (func (export "indirect") (param i32) (result i64)
    (call $prepare)
    (call_indirect (type $v) (local.get 0) (i32.const 0))))
