;; The memory64 proposal's cases import a 64-bit memory and table from the host
;; "spectest" module alongside the 32-bit ones, so this variant of spectest.wat
;; adds those two exports and is otherwise identical. It is a separate module
;; because a module declaring an i64-indexed memory only loads when
;; api.CoreFeatureMemory64 is enabled, which the other suites do not enable.
(module $spectest
  (global (export "global_i32") i32 (i32.const 666))
  (global (export "global_i64") i64 (i64.const 666))
  (global (export "global_f32") f32 (f32.const 666.6))
  (global (export "global_f64") f64 (f64.const 666.6))

  (table (export "table") 10 20 funcref)
  (table (export "table64") i64 10 20 funcref)

  (memory 1 2)
    (export "memory" (memory 0))
  (memory $mem64 i64 1 2)
    (export "memory64" (memory $mem64))

;; Note: the following aren't host functions that print to console as it would clutter it. These only drop the inputs.
  (func)
     (export "print" (func 0))

  (func (param i32) local.get 0 drop)
     (export "print_i32" (func 1))

  (func (param i64) local.get 0 drop)
     (export "print_i64" (func 2))

  (func (param f32) local.get 0 drop)
     (export "print_f32" (func 3))

  (func (param f64) local.get 0 drop)
     (export "print_f64" (func 4))

  (func (param i32 f32) local.get 0 drop local.get 1 drop)
     (export "print_i32_f32" (func 5))

  (func (param f64 f64) local.get 0 drop local.get 1 drop)
     (export "print_f64_f64" (func 6))
)
