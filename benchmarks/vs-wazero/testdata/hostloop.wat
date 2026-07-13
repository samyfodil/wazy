(module
  (import "env" "cb" (func $cb (param i32) (result i32)))
  (func (export "work") (param $n i32) (result i32)
    (local $acc i32) (local $i i32)
    (block $b (loop $c
      (br_if $b (i32.ge_u (local.get $i) (local.get $n)))
      (local.set $acc (i32.add (local.get $acc) (call $cb (local.get $i))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br $c)))
    (local.get $acc)))
