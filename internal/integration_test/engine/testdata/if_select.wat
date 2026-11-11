;; Ifs with short pure arms, which the compiler lowers as selects, and multiplications by powers of two,
;; which it lowers as shifts: see if_select_test.go.
(module
  (func (export "zero") (param $a i32) (param $b i32) (param $c i64) (result i32)
    (if (result i32) (i32.eq (local.get $a) (local.get $b))
      (then (i32.const 0))
      (else (i32.mul (i32.add (local.get $a) (i32.const 2)) (i32.const 8)))))
  (func (export "cmp") (param $a i32) (param $b i32) (param $c i64) (result i32)
    (if (result i32) (local.get $a)
      (then (i32.lt_s (local.get $b) (i32.const 7)))
      (else (i32.eqz (i32.xor (local.get $a) (local.get $b))))))
  (func (export "wide") (param $a i32) (param $b i32) (param $c i64) (result i64)
    (if (result i64) (i32.lt_u (local.get $a) (local.get $b))
      (then (i64.mul (local.get $c) (i64.const 0x100000000)))
      (else (i64.shr_s (local.get $c) (i64.const 3)))))
  ;; Not lowered as selects: an arm too long, one that can trap, and one that sets a local.
  (func (export "long") (param $a i32) (param $b i32) (param $c i64) (result i32)
    (if (result i32) (local.get $a)
      (then (i32.add (i32.add (i32.add (local.get $a) (local.get $b)) (local.get $a)) (local.get $b)))
      (else (local.get $b))))
  (func (export "traps") (param $a i32) (param $b i32) (param $c i64) (result i32)
    (if (result i32) (local.get $a)
      (then (i32.div_s (local.get $b) (local.get $a)))
      (else (i32.const -1))))
  (func (export "sets") (param $a i32) (param $b i32) (param $c i64) (result i32)
    (local $t i32)
    (i32.add
      (if (result i32) (local.get $a)
        (then (local.tee $t (local.get $b)))
        (else (i32.const 5)))
      (local.get $t)))
  ;; Multiplications by powers of two, either way round, by one, by the top bit, and by zero.
  (func (export "pow2") (param $a i32) (param $b i32) (param $c i64) (result i64)
    (i64.add
      (i64.extend_i32_u (i32.add (i32.mul (local.get $a) (i32.const 16)) (i32.mul (i32.const 1) (local.get $b))))
      (i64.add (i64.mul (i64.const 0x8000000000000000) (local.get $c))
        (i64.add (i64.mul (local.get $c) (i64.const 0)) (i64.extend_i32_u (i32.mul (local.get $b) (i32.const 0x80000000)))))))
)
