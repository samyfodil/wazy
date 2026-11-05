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
  ;; A br_if ending its block, after one to that block: not the first, then the second, to the outer label.
  ;; The first an and, the second arithmetic and eqz.
  (func (export "overand") (param $a i32) (param $b i32) (param $c i64) (result i32)
    (block $out
      (block $next
        (br_if $next (i32.and (local.get $a) (i32.const 0x0f0)))
        (br_if $out (i32.eqz (i32.and (local.get $b) (i32.xor (local.get $a) (i32.const -1))))))
      (return (i32.const 4)))
    (i32.const 5))
  ;; The first 64 bits, the second arithmetic to a loop, round again.
  (func (export "overloop") (param $a i32) (param $b i32) (param $c i64) (result i32)
    (local $i i32)
    (loop $top
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (block $skip
        (br_if $skip (i64.gt_s (local.get $c) (i64.extend_i32_s (local.get $i))))
        (br_if $top (i32.lt_u (i32.add (local.get $i) (local.get $b)) (i32.and (local.get $a) (i32.const 511))))))
    (local.get $i))
  ;; Arithmetic in a merged condition to one label.
  (func (export "arith") (param $a i32) (param $b i32) (param $c i64) (result i32)
    (block $out
      (br_if $out (i32.lt_s (local.get $a) (i32.const 1)))
      (br_if $out (i32.gt_u (i32.mul (i32.sub (local.get $a) (local.get $b)) (i32.const 3)) (i32.const 90)))
      (br_if $out (i64.ne (i64.shr_s (local.get $c) (i64.const 1)) (i64.const -3)))
      (return (i32.const 6)))
    (i32.const 7))
  ;; The second br_if carries a value: no merge.
  (func (export "overresult") (param $a i32) (param $b i32) (param $c i64) (result i32)
    (block $out (result i32)
      (block $next
        (br_if $next (i32.and (local.get $a) (local.get $b)))
        (drop (br_if $out (i32.const 8) (i32.eqz (local.get $b)))))
      (i32.const 9))
    (i32.const 1)
    (i32.add))
)
