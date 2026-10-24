;; Funcs aliased out of instances wazy never instantiates, passed on to a
;; nested component with code: in an inline bundle, a func aliased out of a
;; pure re-export shim, and as a bare func arg, the same func aliased back out
;; of the bundle. The shim holds a sibling's export, so both calls land in it:
;; run returns f() + g() = 84.
(component
  (component $provider
    (core module $impl (func (export "f") (result i32) i32.const 42))
    (core instance $impl (instantiate $impl))
    (func (export "f") (result u32) (canon lift (core func $impl "f"))))
  (instance $p (instantiate $provider))
  (alias export $p "f" (func $pf))
  (component $shim
    (import "f" (func $f (result u32)))
    (export "f" (func $f)))
  (instance $s (instantiate $shim (with "f" (func $pf))))
  (alias export $s "f" (func $sf))
  (instance $b (export "f" (func $sf)))
  (alias export $b "f" (func $bf))

  (component $c
    (import "b" (instance $b (export "f" (func (result u32)))))
    (import "g" (func $g (result u32)))
    (alias export $b "f" (func $f))
    (core func $lf (canon lower (func $f)))
    (core func $lg (canon lower (func $g)))
    (core module $m
      (import "" "f" (func $f (result i32)))
      (import "" "g" (func $g (result i32)))
      (func (export "run") (result i32) (i32.add (call $f) (call $g))))
    (core instance $i (instantiate $m
      (with "" (instance (export "f" (func $lf)) (export "g" (func $lg))))))
    (func (export "run") (result u32) (canon lift (core func $i "run"))))
  (instance $ci (instantiate $c (with "b" (instance $b)) (with "g" (func $bf))))
  (export "run" (func $ci "run")))
