(component
  (import "primary" (implements "w:kv/s@0.2") (version-suffix ".6") (instance $p
    (export "get" (func (param "k" u32) (result u32)))))
  (import "secondary" (implements "w:kv/s@0.2") (version-suffix ".6") (instance $s
    (export "get" (func (param "k" u32) (result u32)))))
  (alias export $p "get" (func $pget))
  (alias export $s "get" (func $sget))
  (core func $pget_lowered (canon lower (func $pget)))
  (core func $sget_lowered (canon lower (func $sget)))
  (core instance $host
    (export "primary" (func $pget_lowered))
    (export "secondary" (func $sget_lowered)))
  (core module $main
    (import "host" "primary" (func $p (param i32) (result i32)))
    (import "host" "secondary" (func $s (param i32) (result i32)))
    (func (export "run") (result i32)
      (i32.add (call $p (i32.const 1)) (call $s (i32.const 2)))))
  (core instance $m (instantiate $main (with "host" (instance $host))))
  (func $run (result u32) (canon lift (core func $m "run")))
  (export "run" (func $run)))
