(component
  (import "unused" (func $f (param "x" u32) (result u32)))
  (core module $m
    (func (export "run") (result i32)
      i32.const 42))
  (core instance $ci (instantiate $m))
  (func $run (result u32)
    (canon lift (core func $ci "run")))
  (export "run" (func $run)))
