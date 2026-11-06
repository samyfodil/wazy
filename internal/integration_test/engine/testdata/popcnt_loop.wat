;; Loops that count the bits of a value by clearing its lowest one at a time: see popcnt_loop_test.go.
(module
  (func (export "count") (param $x i32) (param $n i32) (result i32)
    (loop $l
      (local.set $n (i32.add (local.get $n) (i32.const 1)))
      (br_if $l (local.tee $x (i32.and (i32.add (local.get $x) (i32.const -1)) (local.get $x)))))
    (i32.add (i32.mul (local.get $n) (i32.const 1000)) (local.get $x)))
  ;; The clearing first, operands the other way round.
  (func (export "swapped") (param $x i32) (param $n i32) (result i32)
    (loop $l
      (local.set $x (i32.and (local.get $x) (i32.add (i32.const -1) (local.get $x))))
      (local.set $n (i32.add (i32.const 1) (local.get $n)))
      (br_if $l (local.get $x)))
    (i32.add (i32.mul (local.get $n) (i32.const 1000)) (local.get $x)))
  ;; Not the idiom: twice the count.
  (func (export "twice") (param $x i32) (param $n i32) (result i32)
    (loop $l
      (local.set $n (i32.add (local.get $n) (i32.const 2)))
      (br_if $l (local.tee $x (i32.and (i32.add (local.get $x) (i32.const -1)) (local.get $x)))))
    (local.get $n))
  ;; The idiom with a side effect besides: a loop still.
  (global $g (mut i32) (i32.const 0))
  (func (export "other") (param $x i32) (param $n i32) (result i32)
    (loop $l
      (global.set $g (i32.add (global.get $g) (i32.const 1)))
      (local.set $n (i32.add (local.get $n) (i32.const 1)))
      (br_if $l (local.tee $x (i32.and (i32.add (local.get $x) (i32.const -1)) (local.get $x)))))
    (i32.add (local.get $n) (global.get $g)))
)
