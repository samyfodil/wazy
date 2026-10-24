;; Malformed: the nested shim re-exports an import nothing was passed for.
(component
  (component $S
    (import "f" (func (result u32)))
    (export "g" (func 0))
  )
  (instance $s (instantiate $S))
  (alias export $s "g" (func $g))
  (export "g" (func $g))
)
