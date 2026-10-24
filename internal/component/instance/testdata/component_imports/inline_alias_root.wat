;; A func alias of an inline-export instance's member: the member is this
;; component's own lifted func, which is what the export binds.
(component
  (core module $m (func (export "f") (result i32) i32.const 3))
  (core instance $i (instantiate $m))
  (func $f (result u32) (canon lift (core func $i "f")))
  (type $t u8)
  (instance $x (export "t" (type $t)) (export "h" (func $f)))
  (alias export $x "h" (func $h))
  (export "h" (func $h))
)
