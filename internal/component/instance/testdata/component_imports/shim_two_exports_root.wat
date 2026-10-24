;; A func alias of the second of a nested shim's two exports.
(component
  (core module $m
    (func (export "f") (result i32) i32.const 1)
    (func (export "g") (result i32) i32.const 2))
  (core instance $i (instantiate $m))
  (func $f (result u32) (canon lift (core func $i "f")))
  (func $g (result u32) (canon lift (core func $i "g")))
  (component $S
    (import "f" (func (result u32)))
    (import "g" (func (result u32)))
    (export "a" (func 0))
    (export "b" (func 1))
  )
  (instance $s (instantiate $S (with "f" (func $f)) (with "g" (func $g))))
  (alias export $s "b" (func $b))
  (export "b" (func $b))
)
