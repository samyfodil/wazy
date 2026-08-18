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

  ;; The same dot product, but reading both operands from linear memory at an
  ;; address that moves with the loop counter. The constant-operand kernel above
  ;; lets a runtime hoist the widening of both operands out of the loop, so it
  ;; measures loop-invariant code motion as much as the dot lowering; here the
  ;; operands genuinely change every iteration and only the lowering is left.
  (memory 1)
  (data (i32.const 0) "\00\07\0e\15\1c\23\2a\31\38\3f\46\4d\54\5b\62\69\00\05\0a\0f\14\19\1e\23\28\2d\32\37\3c\41\46\4b\0d\14\1b\22\29\30\37\3e\45\4c\53\5a\61\68\6f\76\03\08\0d\12\17\1c\21\26\2b\30\35\3a\3f\44\49\4e\1a\21\28\2f\36\3d\44\4b\52\59\60\67\6e\75\7c\83\06\0b\10\15\1a\1f\24\29\2e\33\38\3d\42\47\4c\51\27\2e\35\3c\43\4a\51\58\5f\66\6d\74\7b\82\89\90\09\0e\13\18\1d\22\27\2c\31\36\3b\40\45\4a\4f\54")
  (func (export "dotmem") (param $n i32) (result i32)
    (local $acc v128) (local $p i32)
    (block $done (loop $l
      (br_if $done (i32.eqz (local.get $n)))
      (local.set $acc (i32x4.relaxed_dot_i8x16_i7x16_add_s
        (v128.load (local.get $p))
        (v128.load offset=16 (local.get $p))
        (local.get $acc)))
      (local.set $p (i32.and (i32.add (local.get $p) (i32.const 32)) (i32.const 96)))
      (local.set $n (i32.sub (local.get $n) (i32.const 1)))
      (br $l)))
    (i32x4.extract_lane 0 (local.get $acc)))
)
