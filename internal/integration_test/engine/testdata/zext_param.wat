;; Loops whose addresses are block parameters, which arm64 uses without zero-extending where every value
;; coming in is: see zext_param_test.go.
(module
  (memory 1)
  (data (i32.const 0) "\01\02\03\04\05\06\07\08\09\0a\0b\0c\0d\0e\0f\10\11\12\13\14\15\16\17\18\19\1a\1b\1c\1d\1e\1f\20")
  ;; The address starts as the function's parameter, which the caller may pass with its top half set.
  (func (export "param") (param $p i32) (param $n i32) (result i32)
    (local $s i32)
    (block $out
      (loop $l
        (br_if $out (i32.eqz (local.get $n)))
        (local.set $s (i32.add (local.get $s) (i32.load8_u (local.get $p))))
        (local.set $p (i32.add (local.get $p) (i32.const 1)))
        (local.set $n (i32.sub (local.get $n) (i32.const 1)))
        (br $l)))
    (local.get $s))
  ;; The address starts as a constant or the parameter masked, and steps down.
  (func (export "masked") (param $p i32) (param $n i32) (result i32)
    (local $s i32) (local $a i32)
    (local.set $a (i32.and (local.get $p) (i32.const 31)))
    (block $out
      (loop $l
        (br_if $out (i32.eqz (local.get $n)))
        (local.set $s (i32.add (local.get $s) (i32.load8_u (local.get $a))))
        (local.set $a (select (i32.const 3) (i32.sub (local.get $a) (i32.const 1)) (i32.eqz (local.get $a))))
        (local.set $n (i32.sub (local.get $n) (i32.const 1)))
        (br $l)))
    (local.get $s))
)
