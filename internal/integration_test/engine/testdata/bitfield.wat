;; Bit fields and shifted operands, which arm64 makes ubfx and shifted-register forms: see bitfield_test.go.
(module
  (func (export "f32") (param $x i32) (param $y i64) (result i32)
    (i32.xor (i32.xor (i32.and (i32.shr_u (local.get $x) (i32.const 14)) (i32.const 1))
                      (i32.and (i32.const 0xff) (i32.shr_u (local.get $x) (i32.const 3))))
      (i32.xor (i32.and (i32.shr_u (local.get $x) (i32.const 1)) (i32.const 0x3fffffff))
               (i32.and (i32.shr_u (local.get $x) (i32.const 31)) (i32.const 1)))))
  (func (export "f64") (param $x i32) (param $y i64) (result i64)
    (i64.add (i64.and (i64.shr_u (local.get $y) (i64.const 40)) (i64.const 0xff))
      (i64.and (i64.shr_u (local.get $y) (i64.const 1)) (i64.const 0x3fffffffffffffff))))
  ;; A mask with a gap, which is no field, and one past the top, which keeps what the shift leaves.
  (func (export "notfield") (param $x i32) (param $y i64) (result i32)
    (i32.add (i32.and (i32.shr_u (local.get $x) (i32.const 4)) (i32.const 5))
      (i32.and (i32.shr_u (local.get $x) (i32.const 28)) (i32.const 0xff))))
  ;; A shift as the first operand of and, or and xor.
  (func (export "first") (param $x i32) (param $y i64) (result i32)
    (i32.add (i32.xor (i32.shr_u (local.get $x) (i32.const 16)) (local.get $x))
      (i32.add (i32.or (i32.shl (local.get $x) (i32.const 3)) (i32.wrap_i64 (local.get $y)))
        (i32.and (i32.shr_s (local.get $x) (i32.const 5)) (i32.wrap_i64 (local.get $y))))))
  ;; The low bits, sign- and zero-extended: by 16, 8 and 32, an odd width, and two amounts that differ.
  (func (export "low") (param $x i32) (param $y i64) (result i32)
    (i32.xor (i32.xor (i32.shr_s (i32.shl (local.get $x) (i32.const 16)) (i32.const 16))
                      (i32.shr_u (i32.shl (local.get $x) (i32.const 24)) (i32.const 24)))
      (i32.xor (i32.shr_s (i32.shl (local.get $x) (i32.const 7)) (i32.const 7))
               (i32.shr_s (i32.shl (local.get $x) (i32.const 3)) (i32.const 4)))))
  (func (export "low64") (param $x i32) (param $y i64) (result i64)
    (i64.add (i64.shr_s (i64.shl (local.get $y) (i64.const 32)) (i64.const 32))
      (i64.shr_u (i64.shl (local.get $y) (i64.const 60)) (i64.const 60))))
  ;; Amounts that differ, on their own.
  (func (export "mismatch") (param $x i32) (param $y i64) (result i32)
    (i32.shr_s (i32.shl (local.get $x) (i32.const 3)) (i32.const 4)))
)
