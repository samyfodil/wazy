;; Loads from two places in a block, whose bounds checks arm64 makes one: see bounds_merge_test.go.
(module
  (memory (export "memory") 1)
  ;; Two loads of unrelated addresses.
  (func (export "two") (param $a i32) (param $b i32) (result i32)
    (i32.sub (i32.load (local.get $a)) (i32.load offset=4 (local.get $b))))
  ;; The second address computed between them, in 64 bits on the way.
  (func (export "computed") (param $a i32) (param $b i32) (result i32)
    (local $x i32)
    (local.set $x (i32.load (local.get $a)))
    (i32.add (local.get $x)
      (i32.load16_u (i32.add (i32.shl (local.get $b) (i32.const 1)) (i32.const 6)))))
  ;; The second address is what the first load reads: it stays a check of its own.
  (func (export "chase") (param $a i32) (param $b i32) (result i32)
    (i32.load (i32.add (i32.and (i32.load (local.get $a)) (i32.const 0xffff)) (local.get $b))))
  ;; A store between: the second check stays after it, and memory shows it ran.
  (func (export "store") (param $a i32) (param $b i32) (result i32)
    (local $x i32)
    (local.set $x (i32.load (local.get $a)))
    (i32.store (i32.const 16) (i32.add (local.get $x) (i32.const 1)))
    (i32.add (local.get $x) (i32.load (local.get $b))))
  ;; Four loads, as the flocking loop makes.
  (func (export "four") (param $a i32) (param $b i32) (result i32)
    (i32.add
      (i32.sub (i32.load offset=4 (local.get $a)) (i32.load offset=4 (local.get $b)))
      (i32.sub (i32.load (local.get $a)) (i32.load (local.get $b)))))
  ;; More arithmetic for the second address than moves up with it.
  (func (export "long") (param $a i32) (param $b i32) (result i32)
    (i32.add (i32.load (local.get $a))
      (i32.load (i32.xor (i32.add (i32.sub (i32.add (i32.sub (i32.add (i32.sub (i32.add (i32.sub
        (local.get $b) (i32.const 1)) (i32.const 2)) (i32.const 3)) (i32.const 4)) (i32.const 5)) (i32.const 6))
        (i32.const 7)) (i32.const 8)) (i32.const 0)))))
  ;; The second address takes one value twice.
  (func (export "shared") (param $a i32) (param $b i32) (result i32)
    (local $t i32)
    (i32.add (i32.load (local.get $a))
      (i32.load (i32.add (local.tee $t (i32.shr_u (local.get $b) (i32.const 1))) (local.get $t)))))
  (data (i32.const 0) "\01\02\03\04\05\06\07\08\09\0a\0b\0c\0d\0e\0f\10")
)
