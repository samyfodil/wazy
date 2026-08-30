;; GC x SIMD in constant expressions -- the two forms wasmtime's const-expr-gc-simd.wast does not reach.
;;
;; A constant expression is evaluated over two parallel stacks, one of words and one of types, and a v128
;; is one type but two words. wasmtime's file covers struct.new_default (no operands) and array.new_fixed;
;; these are struct.new and array.new, which take vector operands and so pop from both stacks unevenly.

;; struct.new with vector fields, and a scalar between them so a miscount shows up as a wrong value
;; rather than as a wrong length.
(module
  (type $s (struct (field v128) (field i64) (field v128)))
  (global $g anyref (struct.new $s
    (v128.const i32x4 1 2 3 4)
    (i64.const 0x0102030405060708)
    (v128.const i32x4 5 6 7 8)))

  (func (export "f0") (result v128) (struct.get $s 0 (ref.cast (ref $s) (global.get $g))))
  (func (export "f1") (result i64)  (struct.get $s 1 (ref.cast (ref $s) (global.get $g))))
  (func (export "f2") (result v128) (struct.get $s 2 (ref.cast (ref $s) (global.get $g))))
)
(assert_return (invoke "f0") (v128.const i32x4 1 2 3 4))
(assert_return (invoke "f1") (i64.const 0x0102030405060708))
(assert_return (invoke "f2") (v128.const i32x4 5 6 7 8))

;; array.new with a vector initialiser: one v128 value, two words, then the i32 length.
(module
  (type $a (array (mut v128)))
  (global $g anyref (array.new $a (v128.const i32x4 9 8 7 6) (i32.const 3)))

  (func (export "len")  (result i32)  (array.len (ref.cast (ref $a) (global.get $g))))
  (func (export "at")   (param i32) (result v128)
    (array.get $a (ref.cast (ref $a) (global.get $g)) (local.get 0)))
)
(assert_return (invoke "len") (i32.const 3))
(assert_return (invoke "at" (i32.const 0)) (v128.const i32x4 9 8 7 6))
(assert_return (invoke "at" (i32.const 2)) (v128.const i32x4 9 8 7 6))

;; array.new_default over a vector element type, and array.new_fixed with a single element -- the
;; boundary cases either side of the multiply.
(module
  (type $a (array (mut v128)))
  (global $d anyref (array.new_default $a (i32.const 2)))
  (global $one anyref (array.new_fixed $a 1 (v128.const i64x2 -1 -1)))

  (func (export "d0") (result v128) (array.get $a (ref.cast (ref $a) (global.get $d)) (i32.const 0)))
  (func (export "d1") (result v128) (array.get $a (ref.cast (ref $a) (global.get $d)) (i32.const 1)))
  (func (export "one-len") (result i32) (array.len (ref.cast (ref $a) (global.get $one))))
  (func (export "one0") (result v128) (array.get $a (ref.cast (ref $a) (global.get $one)) (i32.const 0)))
)
(assert_return (invoke "d0") (v128.const i64x2 0 0))
(assert_return (invoke "d1") (v128.const i64x2 0 0))
(assert_return (invoke "one-len") (i32.const 1))
(assert_return (invoke "one0") (v128.const i64x2 -1 -1))
