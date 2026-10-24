;; A pure re-export shim passed whole to a nested component with code: $C's
;; run calls the shim's times(6, seven()), which are $P's mul and seven.
(component
  (component $P
    (core module $m
      (func (export "mul") (param i32 i32) (result i32) (i32.mul (local.get 0) (local.get 1)))
      (func (export "seven") (result i32) i32.const 7))
    (core instance $i (instantiate $m))
    (func (export "mul") (param "a" u32) (param "b" u32) (result u32) (canon lift (core func $i "mul")))
    (func (export "seven") (result u32) (canon lift (core func $i "seven"))))
  (component $S
    (import "times" (func $t (param "a" u32) (param "b" u32) (result u32)))
    (import "seven" (func $s (result u32)))
    (export "times" (func $t))
    (export "seven" (func $s)))
  (component $C
    (import "calc" (instance $calc
      (export "times" (func (param "a" u32) (param "b" u32) (result u32)))
      (export "seven" (func (result u32)))))
    (alias export $calc "times" (func $times))
    (alias export $calc "seven" (func $seven))
    (core func $times' (canon lower (func $times)))
    (core func $seven' (canon lower (func $seven)))
    (core module $m
      (import "calc" "times" (func $times (param i32 i32) (result i32)))
      (import "calc" "seven" (func $seven (result i32)))
      (func (export "run") (result i32) (call $times (i32.const 6) (call $seven))))
    (core instance $i (instantiate $m
      (with "calc" (instance (export "times" (func $times')) (export "seven" (func $seven'))))))
    (func (export "run") (result u32) (canon lift (core func $i "run"))))
  (instance $p (instantiate $P))
  (alias export $p "mul" (func $mul))
  (alias export $p "seven" (func $seven))
  (instance $s (instantiate $S (with "times" (func $mul)) (with "seven" (func $seven))))
  (instance $c (instantiate $C (with "calc" (instance $s))))
  (export "run" (func $c "run")))
