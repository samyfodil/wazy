;; Malformed for wazy, though valid: the bundle carries a func aliased out of
;; a pure re-export shim, which is never instantiated, so there is nothing to
;; delegate the nested component's import to -- refused as the same func
;; passed as a func arg is.
(component
  (core module $impl (func (export "f") (result i32) i32.const 42))
  (core instance $impl (instantiate $impl))
  (func $lifted (result u32) (canon lift (core func $impl "f")))
  (component $shim
    (import "f" (func $f (result u32)))
    (export "f" (func $f))
  )
  (instance $s (instantiate $shim (with "f" (func $lifted))))
  (alias export $s "f" (func $sf))
  (instance $b (export "f" (func $sf)))
  (component $c
    (import "b" (instance (export "f" (func (result u32)))))
    (core module $m)
    (core instance (instantiate $m))
  )
  (instance (instantiate $c (with "b" (instance $b))))
)
