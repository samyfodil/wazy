;; Allocates in a loop while two i64 locals and a struct reference stay live,
;; so each of its safepoints spills several words; deep runs it at the
;; bottom of a recursion deep enough to grow the stack first. See
;; TestRootRegionIsTheEngines.
(module
  (type $s (struct (field (mut i64))))
  (func $deep (export "deep") (param $d i32) (param $n i32) (result i64)
    (if (result i64) (local.get $d)
      (then (call $deep (i32.sub (local.get $d) (i32.const 1)) (local.get $n)))
      (else (call $f (local.get $n)))))
  (func $f (export "f") (param $n i32) (result i64)
    (local $a i64) (local $b i64) (local $r (ref null $s))
    (local.set $r (struct.new $s (i64.const 7)))
    (loop $l
      (drop (struct.new $s (local.get $a)))
      (local.set $a (i64.add (local.get $a) (i64.const 1)))
      (local.set $b (i64.add (local.get $b) (local.get $a)))
      (br_if $l (local.tee $n (i32.sub (local.get $n) (i32.const 1)))))
    (i64.add (struct.get $s 0 (local.get $r)) (local.get $b))))
