;; Dependent chains of the three relaxed-SIMD instructions where wazy's choice of
;; a single documented result costs it native instructions: relaxed_madd (wazy
;; rounds twice, wasmtime contracts into an FMA), relaxed_min/max (wazy takes the
;; NaN-propagating form, wasmtime the single minps/maxps), and the i8 dot product
;; (wazy widens and reuses i32x4.dot_i16x8_s). Each loop is serial on purpose:
;; the accumulator feeds the next iteration, so the loop measures the latency of
;; one lowering rather than how well the machine hides it.
(module
  (func (export "madd") (param $n i32) (result f32)
    (local $acc v128) (local $b v128) (local $c v128)
    (local.set $acc (v128.const f32x4 1 1 1 1))
    (local.set $b (v128.const f32x4 1.0000001 1.0000001 1.0000001 1.0000001))
    (local.set $c (v128.const f32x4 0.5 0.5 0.5 0.5))
    (block $done (loop $l
      (br_if $done (i32.eqz (local.get $n)))
      (local.set $acc (f32x4.relaxed_madd (local.get $acc) (local.get $b) (local.get $c)))
      (local.set $n (i32.sub (local.get $n) (i32.const 1)))
      (br $l)))
    (f32x4.extract_lane 0 (local.get $acc)))

  (func (export "minmax") (param $n i32) (result f32)
    (local $acc v128) (local $lo v128) (local $hi v128)
    (local.set $acc (v128.const f32x4 3 3 3 3))
    (local.set $lo (v128.const f32x4 1 1 1 1))
    (local.set $hi (v128.const f32x4 5 5 5 5))
    (block $done (loop $l
      (br_if $done (i32.eqz (local.get $n)))
      (local.set $acc (f32x4.relaxed_max (f32x4.relaxed_min (local.get $acc) (local.get $hi)) (local.get $lo)))
      (local.set $n (i32.sub (local.get $n) (i32.const 1)))
      (br $l)))
    (f32x4.extract_lane 0 (local.get $acc)))

  (func (export "dot") (param $n i32) (result i32)
    (local $acc v128) (local $x v128) (local $y v128)
    (local.set $x (v128.const i8x16 1 2 3 4 5 6 7 8 1 2 3 4 5 6 7 8))
    ;; The second operand stays inside the i7 range the instruction is named for.
    ;; Negative lanes there are the one case where implementations may legally
    ;; disagree -- wasmtime reads them unsigned, wazy signed -- and a benchmark
    ;; whose two sides compute different things measures nothing.
    (local.set $y (v128.const i8x16 1 7 2 6 3 5 4 8 1 7 2 6 3 5 4 8))
    (block $done (loop $l
      (br_if $done (i32.eqz (local.get $n)))
      (local.set $acc (i32x4.relaxed_dot_i8x16_i7x16_add_s (local.get $x) (local.get $y) (local.get $acc)))
      (local.set $n (i32.sub (local.get $n) (i32.const 1)))
      (br $l)))
    (i32x4.extract_lane 0 (local.get $acc)))
)
