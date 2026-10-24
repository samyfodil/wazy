;; Two instances of one component in a single store: a trap in one locks the
;; whole store down, so the other refuses entry afterwards too.
(component
  (component $C
    (core module $M
      (func (export "boom") unreachable)
      (func (export "ok") (result i32) (i32.const 7)))
    (core instance $m (instantiate $M))
    (func (export "boom") (canon lift (core func $m "boom")))
    (func (export "ok") (result u32) (canon lift (core func $m "ok"))))
  (instance $a (instantiate $C))
  (instance $b (instantiate $C))
  (export "boom" (func $a "boom"))
  (export "ok" (func $b "ok")))
