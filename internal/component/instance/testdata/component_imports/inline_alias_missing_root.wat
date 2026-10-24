;; Malformed: a func alias of a member the inline-export instance lacks.
(component
  (core module $m (func (export "f") (result i32) i32.const 3))
  (core instance $i (instantiate $m))
  (func $f (result u32) (canon lift (core func $i "f")))
  (instance $x (export "h" (func $f)))
  (alias export $x "nope" (func $h))
  (export "h" (func $h))
)
