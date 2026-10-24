;; A root component that re-exports the instance of its imported component
;; whole, as an interface: `calc#sum`.
(component
  (type $t (list s32 4))
  (import "calc" (component $Calc
    (type $t (list s32 4))
    (import "t" (type $ti (eq $t)))
    (type $f (func (param "xs" $ti) (result s32)))
    (export "sum" (func (type $f)))
  ))
  (instance $calc (instantiate $Calc (with "t" (type $t))))
  (export "calc" (instance $calc))
)
