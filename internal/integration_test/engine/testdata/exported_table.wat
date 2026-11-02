;; Exports an externref table and a funcref table, with functions that read and
;; write them from the guest side, for api.Module.ExportedTable.
(module
  (type $binop (func (param i32 i32) (result i32)))
  (table $ext (export "externs") 2 10 externref)
  (table $fns (export "funcs") 2 funcref)
  (elem (table $fns) (i32.const 0) func $add)
  (func $add (type $binop) (i32.add (local.get 0) (local.get 1)))
  (func (export "get") (param i32) (result externref) (table.get $ext (local.get 0)))
  (func (export "set") (param i32 externref) (table.set $ext (local.get 0) (local.get 1)))
  (func (export "size") (result i32) (table.size $ext))
  (func (export "call") (param i32 i32 i32) (result i32)
    (call_indirect $fns (type $binop) (local.get 1) (local.get 2) (local.get 0)))
)
