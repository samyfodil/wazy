;; A root component importing a component. It declares what it needs of the
;; import -- a type import `t` it will pass, and a `sum` over it -- then
;; instantiates whatever was supplied with its own `t`, and re-exports the
;; instance's `sum`.
(component
  (type $t (list s32 4))
  (import "calc" (component $Calc
    (type $t (list s32 4))
    (import "t" (type $ti (eq $t)))
    (type $f (func (param "xs" $ti) (result s32)))
    (export "sum" (func (type $f)))
  ))
  (instance $calc (instantiate $Calc (with "t" (type $t))))
  (alias export $calc "sum" (func $sum))
  (export "sum" (func $sum))
)
