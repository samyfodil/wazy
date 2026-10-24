;; wasm-tools' fixed-length-lists.wast refuses this component with the type
;; argument (list u32 9) or (list u32) -- "type mismatch for import `x`" --
;; and this is the case it must accept: the argument matches the imported
;; component's (eq (list s32 10)) bound.
(component
  (import "y" (component $c
    (type $t (list s32 10))
    (import "x" (type (eq $t)))
  ))
  (type $x (list s32 10))
  (instance (instantiate $c (with "x" (type $x))))
)
