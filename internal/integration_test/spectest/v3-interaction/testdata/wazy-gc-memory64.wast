;; GC x memory64.
;;
;; array.new_data, array.init_data, array.new_elem and array.init_elem name a *segment*, not a memory,
;; so their offset and length operands stay i32 however wide the module's memories are. A 64-bit memory
;; in the same module must not widen them, and must not truncate the segment reads either.

(module
  (type $bytes (array (mut i8)))
  (type $words (array (mut i32)))

  ;; A 64-bit memory alongside the segments, active at an i64 offset.
  (memory i64 1)
  (data (i64.const 8) "active-at-i64-offset")
  (data $pass "\01\02\03\04\05\06\07\08")

  (func (export "mem64-load") (param i64) (result i32)
    (i32.load8_u (local.get 0)))

  (func (export "new-data") (param i32 i32) (result i32)
    (array.len (array.new_data $bytes $pass (local.get 0) (local.get 1))))

  (func (export "new-data-get") (param i32 i32 i32) (result i32)
    (array.get_u $bytes (array.new_data $bytes $pass (local.get 0) (local.get 1)) (local.get 2)))

  (func (export "init-data-get") (param i32 i32 i32) (result i32)
    (local $a (ref $bytes))
    (local.set $a (array.new_default $bytes (i32.const 8)))
    (array.init_data $bytes $pass (local.get $a) (i32.const 0) (local.get 0) (local.get 1))
    (array.get_u $bytes (local.get $a) (local.get 2)))
)

;; The 64-bit memory works, and its active segment landed at the i64 offset.
(assert_return (invoke "mem64-load" (i64.const 8)) (i32.const 97))   ;; 'a'
(assert_return (invoke "mem64-load" (i64.const 0)) (i32.const 0))

;; Segment reads are unaffected by the memory's width.
(assert_return (invoke "new-data" (i32.const 0) (i32.const 8)) (i32.const 8))
(assert_return (invoke "new-data-get" (i32.const 0) (i32.const 8) (i32.const 7)) (i32.const 8))
(assert_return (invoke "new-data-get" (i32.const 4) (i32.const 4) (i32.const 0)) (i32.const 5))
(assert_return (invoke "init-data-get" (i32.const 2) (i32.const 4) (i32.const 3)) (i32.const 6))

;; Reading past the segment traps as a memory access, not an array one, and does so on the exact
;; boundary rather than after a 32-bit truncation.
(assert_trap (invoke "new-data" (i32.const 0) (i32.const 9)) "out of bounds memory access")
(assert_trap (invoke "new-data" (i32.const 8) (i32.const 1)) "out of bounds memory access")
