;; br_ifs to one label after one another: see brifmerge_test.go.
(module
  ;; Out of [1, b]: an edge check, the second compare of two registers.
  (func (export "range") (param $a i32) (param $b i32) (param $c i64) (result i32)
    (block $out
      (br_if $out (i32.lt_s (local.get $a) (i32.const 1)))
      (br_if $out (i32.gt_s (local.get $a) (local.get $b)))
      (return (i32.const 1)))
    (i32.const 0))
  ;; Three, unsigned, with an immediate up to 31 and one past it.
  (func (export "three") (param $a i32) (param $b i32) (param $c i64) (result i32)
    (block $out
      (br_if $out (i32.ge_u (local.get $a) (local.get $b)))
      (br_if $out (i32.eq (local.get $b) (i32.const 31)))
      (br_if $out (i32.le_u (local.get $a) (i32.const 100)))
      (return (i32.add (local.get $a) (local.get $b))))
    (i32.const -1))
  ;; eqz, a negative constant, and 64 bits.
  (func (export "mixed") (param $a i32) (param $b i32) (param $c i64) (result i32)
    (block $out
      (br_if $out (i32.eqz (local.get $a)))
      (br_if $out (i64.lt_s (local.get $c) (i64.const -5)))
      (br_if $out (i32.ne (local.get $b) (i32.const 7)))
      (return (i32.const 2)))
    (i32.const 3))
  ;; br_ifs to different labels, one after the other: two branches.
  (func (export "twolabels") (param $a i32) (param $b i32) (param $c i64) (result i32)
    (block $two
      (block $one
        (br_if $one (i32.lt_s (local.get $a) (i32.const 1)))
        (br_if $two (i32.gt_s (local.get $a) (local.get $b)))
        (return (i32.const 1)))
      (return (i32.const 10)))
    (i32.const 20))
  ;; In a loop: br_ifs back to the top.
  (func (export "loop") (param $a i32) (param $b i32) (param $c i64) (result i32)
    (local $i i32) (local $n i32)
    (loop $top
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (local.set $n (i32.add (local.get $n) (i32.and (local.get $i) (i32.const 3))))
      (br_if $top (i32.lt_u (local.get $i) (i32.and (local.get $a) (i32.const 255))))
      (br_if $top (i32.lt_s (local.get $n) (i32.and (local.get $b) (i32.const 1023)))))
    (local.get $n))
)
