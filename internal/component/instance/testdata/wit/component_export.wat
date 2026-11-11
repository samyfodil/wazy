;; A component exporting a component, which WIT leaves out.
(component
  (component $c)
  (export "c" (component $c))
  (core module $m (func (export "f")))
  (core instance $i (instantiate $m))
  (func (export "f") (canon lift (core func $i "f"))))
