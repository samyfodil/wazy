;; As bundle_arg_seen_through, but what the shim holds is this component's
;; own lift: a call from the nested component into its parent, which is
;; refused when made, as Wasmtime refuses it.
(component
  (core module $impl (func (export "f") (result i32) i32.const 42))
  (core instance $impl (instantiate $impl))
  (func $lifted (result u32) (canon lift (core func $impl "f")))
  (component $shim
    (import "f" (func $f (result u32)))
    (export "f" (func $f)))
  (instance $s (instantiate $shim (with "f" (func $lifted))))
  (alias export $s "f" (func $sf))
  (instance $b (export "f" (func $sf)))

  (component $c
    (import "b" (instance $b (export "f" (func (result u32)))))
    (alias export $b "f" (func $f))
    (core func $lf (canon lower (func $f)))
    (core module $m
      (import "" "f" (func $f (result i32)))
      (func (export "run") (result i32) (call $f)))
    (core instance $i (instantiate $m (with "" (instance (export "f" (func $lf))))))
    (func (export "run") (result u32) (canon lift (core func $i "run"))))
  (instance $ci (instantiate $c (with "b" (instance $b))))
  (export "run" (func $ci "run")))
