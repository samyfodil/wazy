;; One comparison several selects and branches take: see sharedcmp_test.go.
(module
  (func (export "selects") (param $a i32) (param $b i32) (result i32)
    (local $c i32)
    (local.set $c (i32.gt_u (local.get $a) (local.get $b)))
    (i32.add
      (select (local.get $a) (local.get $b) (local.get $c))
      (i32.mul (select (i32.const 3) (i32.const 5) (local.get $c)) (i32.const 7))))
  ;; Also a value, and a branch.
  (func (export "mixed") (param $a i32) (param $b i32) (result i32)
    (local $c i32) (local $r i32)
    (local.set $c (i32.lt_s (local.get $a) (local.get $b)))
    (local.set $r (i32.add (select (local.get $a) (local.get $b) (local.get $c)) (local.get $c)))
    (block $done
      (br_if $done (local.get $c))
      (local.set $r (i32.mul (local.get $r) (i32.const 3))))
    (local.get $r))
  ;; 64-bit, across a block boundary for one of its users.
  (func (export "wide") (param $a i32) (param $b i32) (result i32)
    (local $c i32) (local $x i64)
    (local.set $c (i64.ge_s (i64.extend_i32_s (local.get $a)) (i64.extend_i32_u (local.get $b))))
    (local.set $x (select (i64.const 10) (i64.const 20) (local.get $c)))
    (if (local.get $b) (then (local.set $x (i64.add (local.get $x) (select (i64.const 1) (i64.const 2) (local.get $c))))))
    (i32.wrap_i64 (local.get $x)))
)
