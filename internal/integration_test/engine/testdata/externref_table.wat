;; Stores an externref in a table and reads it back, so a host value with its
;; high 32 bits set must survive the table on every target.
(module
  (table 1 externref)
  (func (export "round_trip") (param externref) (result externref)
    (table.set (i32.const 0) (local.get 0))
    (table.get (i32.const 0)))
)
