;; Not a subtype of the type calc_root.wat declares: it imports `log`, which
;; the importer never promised to supply.
(component
  (type $t (list s32 4))
  (import "log" (func (param "msg" string)))
  (core module $m
    (func (export "sum") (param i32 i32 i32 i32) (result i32)
      local.get 0 local.get 1 i32.add local.get 2 i32.add local.get 3 i32.add))
  (core instance $i (instantiate $m))
  (func (export "sum") (param "xs" $t) (result s32) (canon lift (core func $i "sum")))
)
