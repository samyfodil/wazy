;; Exports using an imported interface's record and resource with no world
;; use of them: the world gets one (wasm-tools leaves it out).
(component
  (import "test:w/host" (instance $host
    (export "r" (type (sub resource)))
    (type (record (field "a" u32)))
    (export "rec" (type (eq 1)))))
  (alias export $host "r" (type $r))
  (alias export $host "rec" (type $rec))
  (core module $m
    (func (export "keep") (param i32) (result i32) local.get 0)
    (func (export "take") (param i32) (result i32) local.get 0))
  (core instance $i (instantiate $m))
  (func (export "keep") (param "x" (borrow $r)) (result u32)
    (canon lift (core func $i "keep")))
  (func (export "take") (param "x" $rec) (result u32)
    (canon lift (core func $i "take")))
)
