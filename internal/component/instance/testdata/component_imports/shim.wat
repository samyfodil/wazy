;; A pure re-export shim: it exports the func it imports.
(component
  (import "f" (func (result u32)))
  (export "g" (func 0))
)
