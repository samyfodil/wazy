;; Instances of nested components with code of their own, exported whole:
;; `c`, whose component exports a func, becomes the interface c with member
;; f; `d`, whose component exports an interface of its own, cannot be one.
(component
  (component $C
    (core module $m (func (export "f") (result i32) i32.const 1))
    (core instance $i (instantiate $m))
    (func $f (result u32) (canon lift (core func $i "f")))
    (export "f" (func $f))
  )
  (instance $c (instantiate $C))
  (export "c" (instance $c))
)
