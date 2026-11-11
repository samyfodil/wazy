;; Divisions whose quotient is clamped: see clampdiv_test.go.
(module
  ;; clang's: max then min, each a select of the local and the bound, the local teed again each time.
  (func (export "clang") (param $x i32) (param $d i32) (result i32)
    (local $q i32)
    (i32.div_s (local.get $x) (local.get $d))
    local.tee $q
    i32.const -8
    local.get $q
    i32.const -8
    i32.gt_s
    select
    local.tee $q
    i32.const 7
    local.get $q
    i32.const 7
    i32.lt_s
    select
    local.tee $q
    (i32.mul (local.get $q) (i32.const 1000))
    i32.add)
  ;; min first, the selects the other way round, and ge/le.
  (func (export "minfirst") (param $x i32) (param $d i32) (result i32)
    (local $q i32)
    (local.set $q (i32.div_s (local.get $x) (local.get $d)))
    (local.set $q (select (i32.const 100) (local.get $q) (i32.ge_s (local.get $q) (i32.const 100))))
    (local.tee $q (select (i32.const -3) (local.get $q) (i32.le_s (local.get $q) (i32.const -3)))))
;; A full clamp, but a local keeps the clamp's first half, and is read afterwards: no clamped division.
  (func (export "inter") (param $x i32) (param $d i32) (result i32)
    (local $q i32)
    (local.set $q (i32.div_s (local.get $x) (local.get $d)))
    (local.set $q (select (i32.const 100) (local.get $q) (i32.ge_s (local.get $q) (i32.const 100))))
    (select (i32.const -3) (local.get $q) (i32.le_s (local.get $q) (i32.const -3)))
    (i32.add (i32.mul (local.get $q) (i32.const 7))))
  ;; The unclamped quotient is read afterwards: no clamped division.
  (func (export "raw") (param $x i32) (param $d i32) (result i32)
    (local $q i32) (local $r i32)
    (local.set $r (select (local.tee $q (i32.div_s (local.get $x) (local.get $d))) (i32.const 5) (i32.lt_s (local.get $q) (i32.const 5))))
    (i32.add (local.get $r) (i32.mul (local.get $q) (i32.const 7))))
  ;; A clamp to [0, 255], which the division by a positive divisor does not skip: no clamped division.
  (func (export "byte") (param $x i32) (param $d i32) (result i32)
    (local $q i32)
    (local.set $q (i32.div_s (local.get $x) (local.get $d)))
    (local.set $q (select (local.get $q) (i32.const 0) (i32.gt_s (local.get $q) (i32.const 0))))
    (select (local.get $q) (i32.const 255) (i32.lt_s (local.get $q) (i32.const 255))))
  ;; An ADPCM-like encoder loop, the step adapting to each code.
  (func (export "adpcm") (param $n i32) (param $seed i32) (result i32)
    (local $i i32) (local $pred i32) (local $step i32) (local $c i32) (local $h i32) (local $s i32)
    (local.set $step (i32.const 16))
    (local.set $h (i32.const -2128831035))
    (loop $l
      (local.set $s (i32.mul (i32.xor (local.tee $s (i32.add (local.get $i) (local.get $seed))) (i32.shr_u (local.get $s) (i32.const 16))) (i32.const -2048144789)))
      (i32.div_s (i32.sub (i32.and (i32.xor (local.get $s) (i32.shr_u (local.get $s) (i32.const 13))) (i32.const 65535)) (i32.add (local.get $pred) (i32.const 32768))) (local.get $step))
      local.tee $c
      i32.const -8
      local.get $c
      i32.const -8
      i32.gt_s
      select
      local.tee $c
      i32.const 7
      local.get $c
      i32.const 7
      i32.lt_s
      select
      local.set $c
      (local.set $pred (i32.add (local.get $pred) (i32.mul (local.get $c) (local.get $step))))
      (local.set $step (i32.add (local.get $step) (select (i32.const -1) (i32.const 2) (i32.le_u (select (i32.sub (i32.const 0) (local.get $c)) (local.get $c) (i32.lt_s (local.get $c) (i32.const 0))) (i32.const 3)))))
      (local.set $step (select (local.get $step) (i32.const 1) (i32.gt_s (local.get $step) (i32.const 1))))
      (local.set $h (i32.mul (i32.xor (local.get $h) (i32.and (local.get $c) (i32.const 15))) (i32.const 16777619)))
      (br_if $l (i32.ne (local.tee $i (i32.add (local.get $i) (i32.const 1))) (local.get $n))))
    (i32.add (local.get $h) (local.get $pred)))
)
