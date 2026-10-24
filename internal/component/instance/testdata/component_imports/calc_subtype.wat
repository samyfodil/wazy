;; A subtype of the type calc_root.wat declares: it imports less (no `t`; it
;; spells the list itself) and exports more (`product` too).
(component
  (type $t (list s32 4))
  (core module $m
    (func (export "sum") (param i32 i32 i32 i32) (result i32)
      local.get 0 local.get 1 i32.add local.get 2 i32.add local.get 3 i32.add)
    (func (export "product") (param i32 i32 i32 i32) (result i32)
      local.get 0 local.get 1 i32.mul local.get 2 i32.mul local.get 3 i32.mul))
  (core instance $i (instantiate $m))
  (func (export "sum") (param "xs" $t) (result s32) (canon lift (core func $i "sum")))
  (func (export "product") (param "xs" $t) (result s32) (canon lift (core func $i "product")))
)
