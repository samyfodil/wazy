;; 64-bit products of 32-bit values, which arm64 makes smull and umull: see widened_mul_test.go.
(module
  (func (export "signed") (param $a i32) (param $b i32) (result i64)
    (i64.mul (i64.extend_i32_s (local.get $a)) (i64.extend_i32_s (local.get $b))))
  (func (export "unsigned") (param $a i32) (param $b i32) (result i64)
    (i64.mul (i64.extend_i32_u (local.get $a)) (i64.extend_i32_u (local.get $b))))
  ;; Extended differently: no 32-bit product.
  (func (export "mixed") (param $a i32) (param $b i32) (result i64)
    (i64.mul (i64.extend_i32_s (local.get $a)) (i64.extend_i32_u (local.get $b))))
  ;; By constants that fit 32 bits so extended, and that do not.
  (func (export "consts") (param $a i32) (param $b i32) (result i64)
    (i64.xor (i64.xor (i64.mul (i64.extend_i32_s (local.get $a)) (i64.const -2147483647))
                      (i64.mul (i64.const 0xfffffff7) (i64.extend_i32_u (local.get $b))))
      (i64.xor (i64.mul (i64.extend_i32_s (local.get $a)) (i64.const 0x80000003))
               (i64.mul (i64.extend_i32_u (local.get $b)) (i64.const 0x100000005)))))
  ;; Added to and subtracted from a total.
  (func (export "accumulate") (param $a i32) (param $b i32) (result i64)
    (i64.sub
      (i64.add (i64.const 0x123456789) (i64.mul (i64.extend_i32_s (local.get $a)) (i64.extend_i32_s (local.get $b))))
      (i64.mul (i64.extend_i32_u (local.get $b)) (i64.extend_i32_u (local.get $a)))))
)
