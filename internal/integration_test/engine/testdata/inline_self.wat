;; Recursive functions whose calls to themselves the compiler lowers as a copy of their body: see
;; inline_self_test.go.
(module
  (memory 1)

  (func $fib (export "fib") (param $n i32) (result i32)
    (if (result i32) (i32.lt_s (local.get $n) (i32.const 2))
      (then (local.get $n))
      (else (i32.add (call $fib (i32.sub (local.get $n) (i32.const 1)))
                     (call $fib (i32.sub (local.get $n) (i32.const 2)))))))

  ;; Sums memory down from p, reading p itself at each level: an address out of bounds traps in the copy
  ;; as often as in the function.
  (func $sumdown (export "sumdown") (param $p i32) (param $n i32) (result i64)
    (local $acc i64)
    (local.set $acc (i64.load32_u (local.get $p)))
    (if (i32.eqz (local.get $n)) (then (return (local.get $acc))))
    (i64.add (local.get $acc) (call $sumdown (i32.add (local.get $p) (i32.const 4)) (i32.sub (local.get $n) (i32.const 1)))))

  (func $boom (param i32) (result i32)
    (if (i32.eqz (local.get 0)) (then unreachable))
    (local.get 0))

  ;; Calls a function that traps when it reaches zero: the backtrace runs through copies and calls.
  (func $deep (export "deep") (param $n i32) (result i32)
    (block $out
      (br_if $out (i32.eqz (local.get $n)))
      (return (i32.add (i32.const 1) (call $deep (i32.sub (local.get $n) (i32.const 1))))))
    (call $boom (local.get $n)))

  ;; Two results, a local that starts at zero in every copy, and a branch out of the body's own label.
  (func $pair (export "pair") (param $a i32) (param $b i32) (result i32 i32)
    (local $t i32)
    (local.set $t (i32.add (local.get $t) (local.get $a)))
    (block (result i32 i32)
      (local.get $t) (local.get $b)
      (br_if 1 (i32.le_s (local.get $a) (i32.const 0)))
      (call $pair (i32.sub (local.get $a) (i32.const 1)) (i32.add (local.get $b) (local.get $t)))
      (br 1))
    )
)
