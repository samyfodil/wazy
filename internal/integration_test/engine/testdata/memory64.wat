;; A module whose memory and table both use an i64 index type, exercising every
;; instruction whose operands the memory64 proposal widens. Used by
;; memory64_test.go, which runs it on both engines.
(module
  (memory (export "memory") i64 1 4)
  (data (i64.const 0) "hello")

  (table $t i64 3 8 funcref)
  (elem (i64.const 0) func $one $two)
  (type $ret_i32 (func (result i32)))
  (func $one (result i32) (i32.const 100))
  (func $two (result i32) (i32.const 200))

  (func (export "load8") (param i64) (result i32) (i32.load8_u (local.get 0)))
  (func (export "store8") (param i64 i32) (i32.store8 (local.get 0) (local.get 1)))
  (func (export "load64") (param i64) (result i64) (i64.load (local.get 0)))
  (func (export "store64") (param i64 i64) (i64.store (local.get 0) (local.get 1)))
  (func (export "load_off") (param i64) (result i32)
    (i32.load8_u offset=0x1_0000_0000 (local.get 0)))
  (func (export "mem_size") (result i64) (memory.size))
  (func (export "mem_grow") (param i64) (result i64) (memory.grow (local.get 0)))
  (func (export "mem_fill") (param i64 i32 i64) (memory.fill (local.get 0) (local.get 1) (local.get 2)))
  (func (export "mem_copy") (param i64 i64 i64) (memory.copy (local.get 0) (local.get 1) (local.get 2)))
  (data $d "xyz")
  (func (export "mem_init") (param i64 i32 i32) (memory.init $d (local.get 0) (local.get 1) (local.get 2)))

  (func (export "call_indirect") (param i64) (result i32)
    (call_indirect $t (type $ret_i32) (local.get 0)))
  (func (export "table_size") (result i64) (table.size $t))
  (func (export "table_grow") (param i64) (result i64) (table.grow $t (ref.null func) (local.get 0)))
  (func (export "table_is_null") (param i64) (result i32) (ref.is_null (table.get $t (local.get 0))))
  (func (export "table_set_two") (param i64) (table.set $t (local.get 0) (ref.func $two)))
  (func (export "table_fill") (param i64 i64) (table.fill $t (local.get 0) (ref.null func) (local.get 1)))
  (func (export "table_copy") (param i64 i64 i64) (table.copy $t $t (local.get 0) (local.get 1) (local.get 2)))
  (elem $e func $one)
  (func (export "table_init") (param i64 i32 i32) (table.init $t $e (local.get 0) (local.get 1) (local.get 2)))
)
