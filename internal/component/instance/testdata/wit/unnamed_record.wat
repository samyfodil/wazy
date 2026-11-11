;; An export taking a record no interface names, which WIT cannot spell.
(component
  (type $r (record (field "a" u32)))
  (core module $m (func (export "f") (param i32)))
  (core instance $i (instantiate $m))
  (func (export "f") (param "x" $r) (canon lift (core func $i "f"))))
