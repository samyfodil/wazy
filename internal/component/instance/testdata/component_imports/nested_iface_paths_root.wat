;; An interface nested in an exported instance, reached every way a
;; component can reach it: $W exports $D's instance whole, as "d", so its
;; "d" holds $D's "iface". The root exports that interface through a
;; two-level alias ("x"), its func through one ("g"), hands it to $U as an
;; instantiate-arg ("h" calls it and adds one), and exports $W's instance
;; whole in turn ("whole", three levels down to "f").
(component
  (component $D
    (core module $m (func (export "f") (result i32) i32.const 1))
    (core instance $i (instantiate $m))
    (func $f (result u32) (canon lift (core func $i "f")))
    (instance $iface (export "f" (func $f)))
    (export "iface" (instance $iface))
  )
  (component $W
    (component $D
      (core module $m (func (export "f") (result i32) i32.const 1))
      (core instance $i (instantiate $m))
      (func $f (result u32) (canon lift (core func $i "f")))
      (instance $iface (export "f" (func $f)))
      (export "iface" (instance $iface))
    )
    (instance $d (instantiate $D))
    (export "d" (instance $d))
  )
  (component $U
    (import "i" (instance $i (export "f" (func (result u32)))))
    (alias export $i "f" (func $f))
    (core func $fl (canon lower (func $f)))
    (core module $m
      (import "" "f" (func $f (result i32)))
      (func (export "h") (result i32) call $f i32.const 1 i32.add))
    (core instance $ci (instantiate $m (with "" (instance (export "f" (func $fl))))))
    (func $h (result u32) (canon lift (core func $ci "h")))
    (export "h" (func $h))
  )
  (instance $w (instantiate $W))
  (alias export $w "d" (instance $d))
  (alias export $d "iface" (instance $iface))
  (alias export $iface "f" (func $f))
  (instance $u (instantiate $U (with "i" (instance $iface))))
  (alias export $u "h" (func $h))
  (export "x" (instance $iface))
  (export "g" (func $f))
  (export "h" (func $h))
  (export "whole" (instance $w))
)
