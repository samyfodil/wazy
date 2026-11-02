;; A table that starts empty and can grow, for the embedder's table limit.
(module
  (table $t 0 externref)
  (func (export "grow") (param i32) (result i32)
    (table.grow $t (ref.null extern) (local.get 0)))
  (func (export "size") (result i32)
    (table.size $t))
)
