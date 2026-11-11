;; 64x64->128 multiplies as producers spell them, for TestWideMul.
;; multi3 is compiler-rt's __multi3 as clang emits it (pdfium, Zig), multi3_4 the
;; variant with a 64-bit right operand found in libsodium, gomul math/bits.Mul64's form, and
;; notmul the same as gomul with one cross product repeated, which must not fuse. swapmul
;; spells gomul with every operand order flipped, and lo31 a low half shifted by 31,
;; which must not fuse either.
(module
  (memory (export "memory") 1)
  (func (export "multi3") (param i32 i64 i64 i64 i64)
    (local i64)
    local.get 0
    local.get 1
    local.get 4
    i64.mul
    local.get 2
    local.get 3
    i64.mul
    i64.add
    local.get 3
    i64.const 32
    i64.shr_u
    local.tee 2
    local.get 1
    i64.const 32
    i64.shr_u
    local.tee 4
    i64.mul
    i64.add
    local.get 3
    i64.const 4294967295
    i64.and
    local.tee 3
    local.get 1
    i64.const 4294967295
    i64.and
    local.tee 1
    i64.mul
    local.tee 5
    i64.const 32
    i64.shr_u
    local.get 3
    local.get 4
    i64.mul
    i64.add
    local.tee 3
    i64.const 32
    i64.shr_u
    i64.add
    local.get 1
    local.get 2
    i64.mul
    local.get 3
    i64.const 4294967295
    i64.and
    i64.add
    local.tee 1
    i64.const 32
    i64.shr_u
    i64.add
    i64.store offset=8
    local.get 0
    local.get 5
    i64.const 4294967295
    i64.and
    local.get 1
    i64.const 32
    i64.shl
    i64.or
    i64.store
  )
  (func (export "multi3_4") (param i32 i64 i64 i64)
    (local i64 i64 i64 i64)
    local.get 3
    i64.const 4294967295
    i64.and
    local.tee 4
    local.get 1
    i64.const 4294967295
    i64.and
    local.tee 5
    i64.mul
    local.set 6
    local.get 5
    local.get 3
    i64.const 32
    i64.shr_u
    local.tee 7
    i64.mul
    local.get 4
    local.get 1
    i64.const 32
    i64.shr_u
    local.tee 4
    i64.mul
    local.get 6
    i64.const 32
    i64.shr_u
    i64.add
    local.tee 5
    i64.const 4294967295
    i64.and
    i64.add
    local.set 1
    local.get 0
    local.get 2
    local.get 3
    i64.mul
    local.get 4
    local.get 7
    i64.mul
    i64.add
    local.get 5
    i64.const 32
    i64.shr_u
    i64.add
    local.get 1
    i64.const 32
    i64.shr_u
    i64.add
    i64.store offset=8
    local.get 0
    local.get 6
    i64.const 4294967295
    i64.and
    local.get 1
    i64.const 32
    i64.shl
    i64.or
    i64.store
  )
  (func (export "gomul") (param $x i64) (param $y i64) (result i64 i64)
    (local $x0 i64) (local $x1 i64) (local $y0 i64) (local $y1 i64) (local $w0 i64) (local $t i64) (local $w1 i64)
    (local.set $x0 (i64.and (local.get $x) (i64.const 4294967295)))
    (local.set $x1 (i64.shr_u (local.get $x) (i64.const 32)))
    (local.set $y0 (i64.and (local.get $y) (i64.const 4294967295)))
    (local.set $y1 (i64.shr_u (local.get $y) (i64.const 32)))
    (local.set $w0 (i64.mul (local.get $x0) (local.get $y0)))
    (local.set $t (i64.add (i64.mul (local.get $x1) (local.get $y0)) (i64.shr_u (local.get $w0) (i64.const 32))))
    (local.set $w1 (i64.add (i64.and (local.get $t) (i64.const 4294967295)) (i64.mul (local.get $x0) (local.get $y1))))
    (i64.add (i64.add (i64.mul (local.get $x1) (local.get $y1)) (i64.shr_u (local.get $t) (i64.const 32))) (i64.shr_u (local.get $w1) (i64.const 32)))
    (i64.mul (local.get $x) (local.get $y)))
  (func (export "notmul") (param $x i64) (param $y i64) (result i64)
    (local $x0 i64) (local $x1 i64) (local $y0 i64) (local $y1 i64) (local $w0 i64) (local $t i64) (local $w1 i64)
    (local.set $x0 (i64.and (local.get $x) (i64.const 4294967295)))
    (local.set $x1 (i64.shr_u (local.get $x) (i64.const 32)))
    (local.set $y0 (i64.and (local.get $y) (i64.const 4294967295)))
    (local.set $y1 (i64.shr_u (local.get $y) (i64.const 32)))
    (local.set $w0 (i64.mul (local.get $x0) (local.get $y0)))
    (local.set $t (i64.add (i64.mul (local.get $x1) (local.get $y0)) (i64.shr_u (local.get $w0) (i64.const 32))))
    (local.set $w1 (i64.add (i64.and (local.get $t) (i64.const 4294967295)) (i64.mul (local.get $x1) (local.get $y0))))
    (i64.add (i64.add (i64.mul (local.get $x1) (local.get $y1)) (i64.shr_u (local.get $t) (i64.const 32))) (i64.shr_u (local.get $w1) (i64.const 32))))
  (func (export "swapmul") (param $x i64) (param $y i64) (result i64 i64)
    (local $x0 i64) (local $x1 i64) (local $y0 i64) (local $y1 i64) (local $w0 i64) (local $t i64) (local $w1 i64)
    (local.set $x0 (i64.and (i64.const 4294967295) (local.get $x)))
    (local.set $x1 (i64.shr_u (local.get $x) (i64.const 32)))
    (local.set $y0 (i64.and (i64.const 4294967295) (local.get $y)))
    (local.set $y1 (i64.shr_u (local.get $y) (i64.const 32)))
    (local.set $w0 (i64.mul (local.get $y0) (local.get $x0)))
    (local.set $t (i64.add (i64.shr_u (local.get $w0) (i64.const 32)) (i64.mul (local.get $y0) (local.get $x1))))
    (local.set $w1 (i64.add (i64.mul (local.get $y1) (local.get $x0)) (i64.and (i64.const 4294967295) (local.get $t))))
    (i64.add (i64.shr_u (local.get $w1) (i64.const 32)) (i64.add (i64.shr_u (local.get $t) (i64.const 32)) (i64.mul (local.get $y1) (local.get $x1))))
    (i64.or (i64.shl (local.get $w1) (i64.const 32)) (i64.and (local.get $w0) (i64.const 4294967295))))
  (func (export "lo31") (param $x i64) (param $y i64) (result i64)
    (local $x0 i64) (local $x1 i64) (local $y0 i64) (local $y1 i64) (local $w0 i64) (local $t i64) (local $w1 i64)
    (local.set $x0 (i64.and (local.get $x) (i64.const 4294967295)))
    (local.set $x1 (i64.shr_u (local.get $x) (i64.const 32)))
    (local.set $y0 (i64.and (local.get $y) (i64.const 4294967295)))
    (local.set $y1 (i64.shr_u (local.get $y) (i64.const 32)))
    (local.set $w0 (i64.mul (local.get $x0) (local.get $y0)))
    (local.set $t (i64.add (i64.mul (local.get $x1) (local.get $y0)) (i64.shr_u (local.get $w0) (i64.const 32))))
    (local.set $w1 (i64.add (i64.and (local.get $t) (i64.const 4294967295)) (i64.mul (local.get $x0) (local.get $y1))))
    (i64.or (i64.and (local.get $w0) (i64.const 4294967295)) (i64.shl (local.get $w1) (i64.const 31))))
)
