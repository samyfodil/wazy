;; Destructor candidates for checkResourceDtors: one of the type a resource
;; represented by an i64 wants, and one no resource wants.
(module
  (func (export "i64") (param i64))
  (func (export "bad") (param i32) (result i32) local.get 0)
)
