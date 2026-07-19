(module
  (memory (export "memory") 17)
  (func (export "safe") (param i32) (result i32)
    (i32.load (i32.add
      (i32.rem_u (local.get 0) (i32.const 65533))
      (i32.const 1048576))))
  (func (export "unsafe") (param i32) (result i32)
    (i32.load (i32.add
      (i32.rem_u (local.get 0) (i32.const 65534))
      (i32.const 1048576))))
  (func (export "zero") (param i32) (result i32)
    (i32.load (i32.add
      (i32.rem_u (local.get 0) (i32.const 0))
      (i32.const 1048576))))
  (func (export "wide") (param i32) (result i32)
    (i32.load (i32.add
      (i32.rem_u (local.get 0) (i32.const -1))
      (i32.const 1048576))))
  (func (export "scaled") (param i32) (result i32)
    (i32.load (i32.add
      (i32.shl
        (i32.rem_u (local.get 0) (i32.const 16383))
        (i32.const 2))
      (i32.const 1048576))))
  (func (export "scaled_unsafe") (param i32) (result i32)
    (i32.load (i32.add
      (i32.shl
        (i32.rem_u (local.get 0) (i32.const 16385))
        (i32.const 2))
      (i32.const 1048576))))
  (func (export "scaled_overflow") (param i32) (result i32)
    (i32.load (i32.add
      (i32.shl
        (i32.rem_u (local.get 0) (i32.const 1073741825))
        (i32.const 2))
      (i32.const 1048576)))))
