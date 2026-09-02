;; A composed component whose linking interface exports only a TYPE and no
;; funcs. wit-component emits this for any `interface` that declares just a
;; record/resource -- ordinary WIT, and the shape the 36-pair matrix does not
;; contain (every one of its interfaces has functions).
(component
  (component $Provider
    (type $t (resource (rep i32)))
    (component $shim
      (import "t" (type $t' (sub resource)))
      (export "t" (type $t'))
    )
    (instance $si (instantiate $shim (with "t" (type $t))))
    (export "test:x/types@1.0.0" (instance $si))
  )
  (instance $p (instantiate $Provider))
  (alias export $p "test:x/types@1.0.0" (instance $g))
  (component $Consumer
    (import "test:x/types@1.0.0" (instance (export "t" (type (sub resource)))))
    (core module $m (func (export "run") (result i32) (i32.const 42)))
    (core instance $mi (instantiate $m))
    (func (export "run") (result u32) (canon lift (core func $mi "run")))
  )
  (instance $c (instantiate $Consumer (with "test:x/types@1.0.0" (instance $g))))
  (export "run" (func $c "run"))
)
