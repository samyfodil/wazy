;; An instance exported whole whose component exports an interface of its
;; own: an interface of interfaces, which no WIT world has. The interface is
;; reached as "d#iface", the path of names leading to it.
(component
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
