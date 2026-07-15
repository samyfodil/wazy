(component
  (core module $m
    (func (export "run") (result i64)
      i64.const -7))
  (core instance $ci (instantiate $m))
  (func $run (result s64)
    (canon lift (core func $ci "run")))
  (export "run" (func $run)))
