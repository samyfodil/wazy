(module (func (export "spin") (param $n i64) (result i64)
  (block $b (loop $c
    (br_if $b (i64.eqz (local.get $n)))
    (local.set $n (i64.sub (local.get $n) (i64.const 1)))
    (br $c)))
  (local.get $n)))
