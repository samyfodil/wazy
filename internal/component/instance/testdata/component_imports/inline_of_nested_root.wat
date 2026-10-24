;; An inline-export instance assembled from what a nested instance exports,
;; exported as an interface: `i#f` is the nested component's `f`.
(component
  (component $C
    (core module $m (func (export "f") (result i32) i32.const 5))
    (core instance $i (instantiate $m))
    (func $f (result u32) (canon lift (core func $i "f")))
    (export "f" (func $f))
  )
  (instance $c (instantiate $C))
  (alias export $c "f" (func $f))
  (instance $x (export "f" (func $f)))
  (export "i" (instance $x))
)
