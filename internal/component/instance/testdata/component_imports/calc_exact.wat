;; Exactly the type calc_root.wat declares for its import: imports `t`,
;; exports `sum` over it.
(component
  (type $t (list s32 4))
  (import "t" (type $ti (eq $t)))
  (core module $m
    (func (export "sum") (param i32 i32 i32 i32) (result i32)
      local.get 0 local.get 1 i32.add local.get 2 i32.add local.get 3 i32.add))
  (core instance $i (instantiate $m))
  (func (export "sum") (param "xs" $ti) (result s32) (canon lift (core func $i "sum")))
)
