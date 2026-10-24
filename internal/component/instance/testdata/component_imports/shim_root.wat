;; A root component whose component import is satisfied by a pure re-export
;; shim -- a component with no code of its own. It passes the shim its own
;; func, and re-exports the shim's export both through an alias and with the
;; shim's instance whole.
(component
  (core module $m (func (export "f") (result i32) i32.const 7))
  (core instance $i (instantiate $m))
  (func $f (result u32) (canon lift (core func $i "f")))
  (import "shim" (component $S
    (import "f" (func (result u32)))
    (export "g" (func (result u32)))
  ))
  (instance $s (instantiate $S (with "f" (func $f))))
  (export "s" (instance $s))
  (alias export $s "g" (func $g))
  (export "g" (func $g))
)
