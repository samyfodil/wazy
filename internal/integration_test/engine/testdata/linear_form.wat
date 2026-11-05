;; Sums of products of constants, which the compiler rewrites as one coefficient per value: see
;; linear_form_test.go.
(module
  ;; mesh-skinning's shape: sums of scaled values, scaled and summed again.
  (func (export "mesh") (param $a i32) (param $b i32) (param $c i32) (result i32)
    (local $t i32)
    (i32.add
      (i32.add
        (i32.mul
          (i32.add
            (local.tee $t (i32.mul (local.get $c) (i32.const -11)))
            (i32.add (i32.shl (local.get $a) (i32.const 5)) (i32.mul (local.get $b) (i32.const -9))))
          (i32.const 3))
        (i32.shl
          (i32.add (i32.mul (local.get $c) (i32.const 21))
            (i32.add (i32.shl (local.get $a) (i32.const 3)) (i32.mul (local.get $b) (i32.const 7))))
          (i32.const 1)))
      (i32.add (i32.mul (local.get $t) (local.get $a)) (i32.const -251))))
  (func (export "mesh64") (param $a i64) (param $b i64) (param $c i64) (result i64)
    (i64.sub
      (i64.mul
        (i64.add (i64.mul (local.get $a) (i64.const 0x100000001)) (i64.mul (local.get $b) (i64.const -7)))
        (i64.const 0x7fffffffffff))
      (i64.shl (i64.mul (local.get $c) (i64.const 9)) (i64.const 2))))
  ;; A shift by the width or more shifts by what is left of it.
  (func (export "wide") (param $a i32) (param $b i32) (param $c i32) (result i32)
    (i32.add (i32.mul (i32.shl (local.get $a) (i32.const 33)) (i32.const 5))
      (i32.mul (i32.shl (local.get $b) (i32.const 36)) (i32.const 3))))
  ;; Terms that cancel, to one value, and to a constant.
  (func (export "cancel") (param $a i32) (param $b i32) (param $c i32) (result i32)
    (i32.sub (i32.mul (local.get $a) (i32.const 3)) (i32.add (i32.mul (local.get $a) (i32.const 2)) (i32.mul (local.get $b) (i32.const 0)))))
  (func (export "constant") (param $a i32) (param $b i32) (param $c i32) (result i32)
    (i32.add (i32.sub (i32.mul (local.get $a) (i32.const 6)) (i32.mul (i32.mul (local.get $a) (i32.const 2)) (i32.const 3))) (i32.const 77)))
  ;; Every term subtracted, with and without a constant.
  (func (export "negative") (param $a i32) (param $b i32) (param $c i32) (result i32)
    (i32.sub (i32.sub (i32.const 0) (i32.mul (i32.mul (local.get $a) (i32.const 2)) (i32.const 2)))
      (i32.mul (i32.mul (local.get $b) (i32.const 3)) (i32.const 5))))
  (func (export "negconst") (param $a i32) (param $b i32) (param $c i32) (result i32)
    (i32.sub (i32.sub (i32.const 40) (i32.mul (i32.mul (local.get $a) (i32.const -1)) (i32.const -1)))
      (i32.mul (i32.mul (local.get $b) (i32.const 8)) (i32.const 1))))
  ;; A scaled value that another instruction takes too, in a loop.
  (func (export "shared") (param $a i32) (param $b i32) (param $c i32) (result i32)
    (local $i i32) (local $s i32) (local $t i32)
    (loop $l
      (local.set $t (i32.mul (i32.add (local.get $a) (local.get $i)) (i32.const 13)))
      (local.set $s (i32.xor (local.get $s)
        (i32.add (i32.mul (i32.add (local.get $t) (i32.mul (local.get $b) (i32.const 3))) (i32.const 7))
          (i32.mul (local.get $t) (i32.const 5)))))
      (local.set $s (i32.add (local.get $s) (local.get $t)))
      (br_if $l (i32.lt_u (local.tee $i (i32.add (local.get $i) (i32.const 1))) (i32.and (local.get $c) (i32.const 15)))))
    (local.get $s))
  ;; One multiplication, and two by no constant: nothing to gain.
  (func (export "few") (param $a i32) (param $b i32) (param $c i32) (result i32)
    (i32.add (i32.mul (local.get $a) (i32.const 3)) (i32.shl (local.get $b) (i32.const 2))))
  (func (export "varmul") (param $a i32) (param $b i32) (param $c i32) (result i32)
    (i32.add (i32.mul (local.get $a) (local.get $b)) (i32.mul (local.get $b) (local.get $c))))
  ;; The constant first.
  (func (export "leftconst") (param $a i32) (param $b i32) (param $c i32) (result i32)
    (i32.add (i32.mul (i32.const 7) (i32.mul (i32.const 5) (local.get $a))) (i32.mul (i32.const 3) (local.get $b))))
  ;; A sum rewritten before another takes it twice, scaled and not.
  (func (export "twice") (param $a i32) (param $b i32) (param $c i32) (result i32)
    (local $t i32)
    (i32.add
      (i32.mul (local.tee $t (i32.add (i32.mul (i32.mul (local.get $a) (i32.const 3)) (i32.const 5)) (i32.mul (local.get $b) (i32.const 5)))) (i32.const 7))
      (i32.mul (local.get $t) (local.get $c))))
  ;; Too many instructions to take as one.
  (func (export "long") (param $a i32) (param $b i32) (param $c i32) (result i32)
    (i32.add (i32.mul (local.get $a) (i32.const 1)) (i32.add (i32.mul (local.get $a) (i32.const 2)) (i32.add (i32.mul (local.get $a) (i32.const 3)) (i32.add (i32.mul (local.get $a) (i32.const 4)) (i32.add (i32.mul (local.get $a) (i32.const 5)) (i32.add (i32.mul (local.get $a) (i32.const 6)) (i32.add (i32.mul (local.get $a) (i32.const 7)) (i32.add (i32.mul (local.get $a) (i32.const 8)) (i32.add (i32.mul (local.get $a) (i32.const 9)) (i32.add (i32.mul (local.get $a) (i32.const 10)) (i32.add (i32.mul (local.get $a) (i32.const 11)) (i32.add (i32.mul (local.get $a) (i32.const 12)) (i32.add (i32.mul (local.get $a) (i32.const 13)) (i32.add (i32.mul (local.get $a) (i32.const 14)) (i32.add (i32.mul (local.get $a) (i32.const 15)) (i32.add (i32.mul (local.get $a) (i32.const 16)) (i32.add (i32.mul (local.get $a) (i32.const 17)) (i32.add (i32.mul (local.get $a) (i32.const 18)) (i32.add (i32.mul (local.get $a) (i32.const 19)) (i32.add (i32.mul (local.get $a) (i32.const 20)) (i32.add (i32.mul (local.get $a) (i32.const 21)) (i32.add (i32.mul (local.get $a) (i32.const 22)) (i32.add (i32.mul (local.get $a) (i32.const 23)) (i32.add (i32.mul (local.get $a) (i32.const 24)) (i32.add (i32.mul (local.get $a) (i32.const 25)) (i32.add (i32.mul (local.get $a) (i32.const 26)) (i32.add (i32.mul (local.get $a) (i32.const 27)) (i32.add (i32.mul (local.get $a) (i32.const 28)) (i32.add (i32.mul (local.get $a) (i32.const 29)) (i32.add (i32.mul (local.get $a) (i32.const 30)) (i32.add (i32.mul (local.get $a) (i32.const 31)) (i32.add (i32.mul (local.get $a) (i32.const 32)) (i32.add (i32.mul (local.get $a) (i32.const 33)) (i32.add (i32.mul (local.get $a) (i32.const 34)) (i32.add (i32.mul (local.get $a) (i32.const 35)) (i32.add (i32.mul (local.get $a) (i32.const 36)) (i32.add (i32.mul (local.get $a) (i32.const 37)) (i32.add (i32.mul (local.get $a) (i32.const 38)) (i32.add (i32.mul (local.get $a) (i32.const 39)) (i32.mul (local.get $a) (i32.const 40))))))))))))))))))))))))))))))))))))))))))
  ;; As many multiplications and instructions either way.
  (func (export "nogain") (param $a i32) (param $b i32) (param $c i32) (result i32)
    (i32.add (i32.mul (i32.add (local.get $a) (i32.const 1)) (i32.const 3)) (i32.mul (local.get $b) (i32.const 5))))
  ;; A coefficient that is a power of two, and one of minus one alone.
  (func (export "pow2") (param $a i32) (param $b i32) (param $c i32) (result i32)
    (i32.add (i32.add (i32.mul (local.get $a) (i32.const 3)) (i32.mul (local.get $a) (i32.const 5))) (i32.mul (local.get $b) (i32.const 7))))
  (func (export "minusone") (param $a i32) (param $b i32) (param $c i32) (result i32)
    (i32.add (i32.sub (i32.mul (local.get $a) (i32.const 3)) (i32.mul (local.get $a) (i32.const 4)))
      (i32.sub (i32.mul (local.get $b) (i32.const 6)) (i32.mul (local.get $b) (i32.const 6)))))
)
