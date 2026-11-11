;; An export that takes a borrow of a host resource and returns without
;; dropping it: the call must fail, as the reference's
;; trap_if(num_borrows > 0) at task exit does.
(component
  (import "test:lend/host" (instance $host (export "r" (type (sub resource)))))
  (alias export $host "r" (type $r))
  (core module $m
    (func (export "keep") (param i32) (result i32) local.get 0))
  (core instance $i (instantiate $m))
  (func (export "keep") (param "x" (borrow $r)) (result u32)
    (canon lift (core func $i "keep")))
)
