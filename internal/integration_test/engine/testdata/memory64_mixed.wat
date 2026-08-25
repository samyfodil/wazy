;; A module mixing index types: memory/table 0 are i32-indexed, memory/table 1
;; are i64-indexed. memory.copy and table.copy across the pair take their length
;; operand in the narrower of the two index types, in both directions.
(module
  (memory $m32 (export "m32") 1)
  (memory $m64 (export "m64") i64 1)
  (data (memory $m32) (i32.const 0) "abcd")
  (data (memory $m64) (i64.const 0) "wxyz")

  (table $t32 4 funcref)
  (table $t64 i64 4 funcref)
  (type $ret_i32 (func (result i32)))
  (func $one (result i32) (i32.const 100))
  (func $two (result i32) (i32.const 200))
  (elem (table $t32) (i32.const 0) func $one)
  (elem (table $t64) (i64.const 0) func $two)

  (func (export "load32") (param i32) (result i32) (i32.load8_u $m32 (local.get 0)))
  (func (export "load64") (param i64) (result i32) (i32.load8_u $m64 (local.get 0)))

  ;; dst i64, src i32: [i64, i32, i32]
  (func (export "copy_64_from_32") (param i64 i32 i32)
    (memory.copy $m64 $m32 (local.get 0) (local.get 1) (local.get 2)))
  ;; dst i32, src i64: [i32, i64, i32]
  (func (export "copy_32_from_64") (param i32 i64 i32)
    (memory.copy $m32 $m64 (local.get 0) (local.get 1) (local.get 2)))

  (func (export "call32") (param i32) (result i32) (call_indirect $t32 (type $ret_i32) (local.get 0)))
  (func (export "call64") (param i64) (result i32) (call_indirect $t64 (type $ret_i32) (local.get 0)))

  ;; dst i64, src i32: [i64, i32, i32]
  (func (export "tcopy_64_from_32") (param i64 i32 i32)
    (table.copy $t64 $t32 (local.get 0) (local.get 1) (local.get 2)))
  ;; dst i32, src i64: [i32, i64, i32]
  (func (export "tcopy_32_from_64") (param i32 i64 i32)
    (table.copy $t32 $t64 (local.get 0) (local.get 1) (local.get 2)))
)
