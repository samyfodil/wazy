;; A nested component re-exporting the instance it imports, given the root's
;; host import for it: the funcs are the host's, which no export can run.
(component
  (import "host" (instance $host (export "f" (func (result u32)))))
  (component $Fwd
    (import "svc" (instance $svc (export "f" (func (result u32)))))
    (export "svc" (instance $svc)))
  (instance $fwd (instantiate $Fwd (with "svc" (instance $host))))
  (alias export $fwd "svc" (instance $s))
  (export "svc" (instance $s)))
