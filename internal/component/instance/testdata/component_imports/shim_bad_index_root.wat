;; Malformed: the nested shim exports a func index past its imports.
(component
  (core module $m (func (export "f") (result i32) i32.const 7))
  (core instance $i (instantiate $m))
  (func $f (result u32) (canon lift (core func $i "f")))
  (component $S
    (import "f" (func (result u32)))
    (export "g" (func 5))
  )
  (instance $s (instantiate $S (with "f" (func $f))))
  (alias export $s "g" (func $g))
  (export "g" (func $g))
)
