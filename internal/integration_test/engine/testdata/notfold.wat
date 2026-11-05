;; Bitwise operations of a value inverted with xor -1: see notfold_test.go.
(module
  (func (export "bic") (param $a i64) (param $b i64) (result i64)
    (i64.extend_i32_u (i32.and (i32.wrap_i64 (local.get $a)) (i32.xor (i32.wrap_i64 (local.get $b)) (i32.const -1)))))
  (func (export "bic_left") (param $a i64) (param $b i64) (result i64)
    (i64.and (i64.xor (i64.const -1) (local.get $b)) (local.get $a)))
  (func (export "orn") (param $a i64) (param $b i64) (result i64)
    (i64.or (local.get $a) (i64.xor (local.get $b) (i64.const -1))))
  (func (export "eon") (param $a i64) (param $b i64) (result i64)
    (i64.extend_i32_u (i32.xor (i32.wrap_i64 (local.get $a)) (i32.xor (i32.wrap_i64 (local.get $b)) (i32.const -1)))))
  (func (export "mvn") (param $a i64) (param $b i64) (result i64)
    (i64.add (i64.xor (local.get $a) (i64.const -1)) (i64.extend_i32_u (i32.xor (i32.wrap_i64 (local.get $b)) (i32.const -1)))))
  ;; a & ~b == 0 as a branch: bics.
  (func (export "subset") (param $a i64) (param $b i64) (result i64)
    (if (result i64) (i32.eqz (i32.and (i32.wrap_i64 (local.get $a)) (i32.xor (i32.wrap_i64 (local.get $b)) (i32.const -1))))
      (then (i64.const 1)) (else (i64.const 2))))
  ;; The inverted value is used twice: computed once.
  (func (export "shared") (param $a i64) (param $b i64) (result i64)
    (local $n i64)
    (local.set $n (i64.xor (local.get $b) (i64.const -1)))
    (i64.add (i64.and (local.get $a) (local.get $n)) (i64.mul (local.get $n) (i64.const 3))))
  ;; A constant inverted.
  (func (export "constant") (param $a i64) (param $b i64) (result i64)
    (i64.and (local.get $a) (i64.xor (i64.const 0xff00) (i64.const -1))))
)
