;; 32-bit results returned over a parameter slot that held all ones: in registers, and many enough that
;; the last ones go on the stack.
(module
  (func (export "i32") (param i64) (result i32) (i32.wrap_i64 (local.get 0)))
  (func (export "f32") (param i64) (result f32) (f32.reinterpret_i32 (i32.wrap_i64 (local.get 0))))
  (func (export "many") (param i64 i64 i64 i64 i64 i64 i64 i64 i64 i64 i64 i64 i64 i64 i64 i64 i64 i64 i64 i64)
    (result i32 f32 i32 f32 i32 f32 i32 f32 i32 f32 i32 f32 i32 f32 i32 f32 i32 f32 i32 f32)
    (i32.const 1) (f32.const 2) (i32.const 3) (f32.const 4) (i32.const 5) (f32.const 6) (i32.const 7) (f32.const 8)
    (i32.const 9) (f32.const 10) (i32.const 11) (f32.const 12) (i32.const 13) (f32.const 14) (i32.const 15) (f32.const 16)
    (i32.const 17) (f32.const 18) (i32.const 19) (f32.const 20)))
