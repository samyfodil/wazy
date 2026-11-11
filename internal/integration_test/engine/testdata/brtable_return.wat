;; br_tables of a few labels, one the function's own: see brtable_return_test.go.
(module
  (global $g (mut i32) (i32.const 0))
  (func $f (export "f") (param $i i32)
    (block $b
      (br_table $b 1 $b (local.get $i)))
    (global.set $g (i32.add (global.get $g) (i32.const 1))))
  (func (export "g") (param $i i32) (result i32)
    (call $f (local.get $i))
    (call $f (i32.const 1))
    (block $b (result i32)
      (br_table 1 $b 1 (i32.const 9) (local.get $i)))
    (global.get $g)
    i32.add)
)
