;; GC x relaxed SIMD.
;;
;; A v128 in a GC object is two words: a struct lays its fields out in words (see FunctionType.FieldSlots)
;; and an array strides its elements by them. The relaxed vector instructions are the ones whose result the
;; specification leaves open, so an engine may lower them either way -- which makes them the ones most
;; likely to be fed a half-read operand and still look plausible. Every case here reads its operands out of
;; a struct or an array and writes the result back into one.
;;
;; Where a relaxed instruction has more than one permitted result the assertion lists both, which is what
;; the harness's "either" support is for.

(module
  (type $pair (struct (field (mut v128)) (field (mut v128)) (field (mut v128))))
  (type $vecs (array (mut v128)))

  (global $s (mut (ref null $pair)) (ref.null $pair))
  (global $a (mut (ref null $vecs)) (ref.null $vecs))

  (func (export "init")
    (global.set $s (struct.new_default $pair))
    (global.set $a (array.new_default $vecs (i32.const 4))))

  (func (export "set-field") (param $i i32) (param $v v128)
    (block
      (br_if 0 (i32.ne (local.get $i) (i32.const 0)))
      (struct.set $pair 0 (global.get $s) (local.get $v))
      (return))
    (block
      (br_if 0 (i32.ne (local.get $i) (i32.const 1)))
      (struct.set $pair 1 (global.get $s) (local.get $v))
      (return))
    (struct.set $pair 2 (global.get $s) (local.get $v)))
  (func (export "get-field") (param $i i32) (result v128)
    (block
      (br_if 0 (i32.ne (local.get $i) (i32.const 0)))
      (return (struct.get $pair 0 (global.get $s))))
    (block
      (br_if 0 (i32.ne (local.get $i) (i32.const 1)))
      (return (struct.get $pair 1 (global.get $s))))
    (struct.get $pair 2 (global.get $s)))

  (func (export "set-elem") (param $i i32) (param $v v128)
    (array.set $vecs (global.get $a) (local.get $i) (local.get $v)))
  (func (export "get-elem") (param $i i32) (result v128)
    (array.get $vecs (global.get $a) (local.get $i)))

  ;; Read two struct fields, apply a relaxed instruction, store into the third.
  (func (export "s-madd")
    (struct.set $pair 2 (global.get $s)
      (f32x4.relaxed_madd
        (struct.get $pair 0 (global.get $s))
        (struct.get $pair 1 (global.get $s))
        (struct.get $pair 2 (global.get $s)))))
  (func (export "s-min")
    (struct.set $pair 2 (global.get $s)
      (f32x4.relaxed_min
        (struct.get $pair 0 (global.get $s))
        (struct.get $pair 1 (global.get $s)))))
  (func (export "s-laneselect")
    (struct.set $pair 2 (global.get $s)
      (i8x16.relaxed_laneselect
        (struct.get $pair 0 (global.get $s))
        (struct.get $pair 1 (global.get $s))
        (struct.get $pair 2 (global.get $s)))))

  ;; The same across array elements, whose stride is what a miscount would get wrong.
  (func (export "a-swizzle")
    (array.set $vecs (global.get $a) (i32.const 3)
      (i8x16.relaxed_swizzle
        (array.get $vecs (global.get $a) (i32.const 0))
        (array.get $vecs (global.get $a) (i32.const 1)))))
  (func (export "a-trunc")
    (array.set $vecs (global.get $a) (i32.const 3)
      (i32x4.relaxed_trunc_f32x4_s (array.get $vecs (global.get $a) (i32.const 0)))))
  (func (export "a-q15mulr")
    (array.set $vecs (global.get $a) (i32.const 3)
      (i16x8.relaxed_q15mulr_s
        (array.get $vecs (global.get $a) (i32.const 0))
        (array.get $vecs (global.get $a) (i32.const 1)))))
  (func (export "a-dot")
    (array.set $vecs (global.get $a) (i32.const 3)
      (i16x8.relaxed_dot_i8x16_i7x16_s
        (array.get $vecs (global.get $a) (i32.const 0))
        (array.get $vecs (global.get $a) (i32.const 1)))))
)

(assert_return (invoke "init"))

;; A vector survives a round trip through a struct field and an array element intact -- both halves.
(assert_return (invoke "set-field" (i32.const 0) (v128.const i64x2 0x0123456789abcdef 0xfedcba9876543210)))
(assert_return (invoke "get-field" (i32.const 0)) (v128.const i64x2 0x0123456789abcdef 0xfedcba9876543210))
(assert_return (invoke "set-elem" (i32.const 2) (v128.const i64x2 0x1122334455667788 0x99aabbccddeeff00)))
(assert_return (invoke "get-elem" (i32.const 2)) (v128.const i64x2 0x1122334455667788 0x99aabbccddeeff00))
;; Its neighbours are untouched, which is what proves the stride.
(assert_return (invoke "get-elem" (i32.const 1)) (v128.const i64x2 0 0))
(assert_return (invoke "get-elem" (i32.const 3)) (v128.const i64x2 0 0))
(assert_return (invoke "get-field" (i32.const 1)) (v128.const i64x2 0 0))

;; relaxed_madd over struct fields: 2*3+4 = 10, or the same fused.
(assert_return (invoke "set-field" (i32.const 0) (v128.const f32x4 2 2 2 2)))
(assert_return (invoke "set-field" (i32.const 1) (v128.const f32x4 3 3 3 3)))
(assert_return (invoke "set-field" (i32.const 2) (v128.const f32x4 4 4 4 4)))
(assert_return (invoke "s-madd"))
(assert_return (invoke "get-field" (i32.const 2)) (v128.const f32x4 10 10 10 10))

;; relaxed_min over struct fields, away from the NaN and zero cases the specification leaves open.
(assert_return (invoke "set-field" (i32.const 0) (v128.const f32x4 1 5 -3 7)))
(assert_return (invoke "set-field" (i32.const 1) (v128.const f32x4 4 2 -8 7)))
(assert_return (invoke "s-min"))
(assert_return (invoke "get-field" (i32.const 2)) (v128.const f32x4 1 2 -8 7))

;; relaxed_laneselect with lanes that are all-ones or all-zero, where every implementation agrees.
(assert_return (invoke "set-field" (i32.const 0) (v128.const i8x16 1 1 1 1 1 1 1 1 1 1 1 1 1 1 1 1)))
(assert_return (invoke "set-field" (i32.const 1) (v128.const i8x16 2 2 2 2 2 2 2 2 2 2 2 2 2 2 2 2)))
(assert_return (invoke "set-field" (i32.const 2)
  (v128.const i8x16 0xff 0 0xff 0 0xff 0 0xff 0 0xff 0 0xff 0 0xff 0 0xff 0)))
(assert_return (invoke "s-laneselect"))
(assert_return (invoke "get-field" (i32.const 2)) (v128.const i8x16 1 2 1 2 1 2 1 2 1 2 1 2 1 2 1 2))

;; relaxed_swizzle over array elements, with in-range indices only.
(assert_return (invoke "set-elem" (i32.const 0)
  (v128.const i8x16 10 11 12 13 14 15 16 17 18 19 20 21 22 23 24 25)))
(assert_return (invoke "set-elem" (i32.const 1)
  (v128.const i8x16 15 14 13 12 11 10 9 8 7 6 5 4 3 2 1 0)))
(assert_return (invoke "a-swizzle"))
(assert_return (invoke "get-elem" (i32.const 3))
  (v128.const i8x16 25 24 23 22 21 20 19 18 17 16 15 14 13 12 11 10))

;; relaxed_trunc over an array element, in range so the result is defined.
(assert_return (invoke "set-elem" (i32.const 0) (v128.const f32x4 1.5 -2.5 3.9 -4.9)))
(assert_return (invoke "a-trunc"))
(assert_return (invoke "get-elem" (i32.const 3)) (v128.const i32x4 1 -2 3 -4))

;; relaxed_q15mulr and relaxed_dot over array elements, away from the saturating corner.
(assert_return (invoke "set-elem" (i32.const 0) (v128.const i16x8 0x4000 0x4000 0 0 1 1 1 1)))
(assert_return (invoke "set-elem" (i32.const 1) (v128.const i16x8 0x4000 0x4000 0 0 1 1 1 1)))
(assert_return (invoke "a-q15mulr"))
(assert_return (invoke "get-elem" (i32.const 3)) (v128.const i16x8 0x2000 0x2000 0 0 0 0 0 0))

(assert_return (invoke "set-elem" (i32.const 0) (v128.const i8x16 1 2 1 2 1 2 1 2 1 2 1 2 1 2 1 2)))
(assert_return (invoke "set-elem" (i32.const 1) (v128.const i8x16 3 4 3 4 3 4 3 4 3 4 3 4 3 4 3 4)))
(assert_return (invoke "a-dot"))
(assert_return (invoke "get-elem" (i32.const 3)) (v128.const i16x8 11 11 11 11 11 11 11 11))
