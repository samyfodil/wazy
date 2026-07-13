;; dispatch.wat — call_indirect dispatch benchmark kernel for Phase 0 validation.
;; Eight small "virtual methods" of the same type in a funcref table. Each does
;; a small multiply-add chain (~representative of a tiny virtual method / vtable
;; slot). Three hot loops over the same callees:
;;   mono   : always call_indirect table[0]        (monomorphic — IC-favorable)
;;   poly   : call_indirect table[i & 7]           (round-robin — megamorphic)
;;   direct : direct `call $m0`                     (dispatch-free floor)
;; The (mono - direct) delta is the ceiling of what a monomorphic inline cache
;; can recover; (poly) shows the megamorphic case an IC must not regress.
(module
  (type $sig (func (param i32) (result i32)))
  (table 9 funcref)
  (elem (i32.const 0) $m0 $m1 $m2 $m3 $m4 $m5 $m6 $m7 $mh)

  (func $m0 (type $sig) (i32.add (i32.mul (local.get 0) (i32.const 3)) (i32.const 1)))
  (func $m1 (type $sig) (i32.add (i32.mul (local.get 0) (i32.const 5)) (i32.const 2)))
  (func $m2 (type $sig) (i32.add (i32.mul (local.get 0) (i32.const 7)) (i32.const 3)))
  (func $m3 (type $sig) (i32.add (i32.mul (local.get 0) (i32.const 11)) (i32.const 4)))
  (func $m4 (type $sig) (i32.add (i32.mul (local.get 0) (i32.const 13)) (i32.const 5)))
  (func $m5 (type $sig) (i32.add (i32.mul (local.get 0) (i32.const 17)) (i32.const 6)))
  (func $m6 (type $sig) (i32.add (i32.mul (local.get 0) (i32.const 19)) (i32.const 7)))
  (func $m7 (type $sig) (i32.add (i32.mul (local.get 0) (i32.const 23)) (i32.const 8)))

  ;; $mh — a heavier "method" (~16 mul/add ops) representing a small-but-real
  ;; virtual method body. Used by mono_heavy to measure how the fixed dispatch
  ;; overhead dilutes as callee work grows.
  (func $mh (type $sig)
    local.get 0
    i32.const 3 i32.mul i32.const 1 i32.add
    i32.const 3 i32.mul i32.const 1 i32.add
    i32.const 3 i32.mul i32.const 1 i32.add
    i32.const 3 i32.mul i32.const 1 i32.add
    i32.const 3 i32.mul i32.const 1 i32.add
    i32.const 3 i32.mul i32.const 1 i32.add
    i32.const 3 i32.mul i32.const 1 i32.add
    i32.const 3 i32.mul i32.const 1 i32.add)

  ;; monomorphic: every call goes through table slot 0.
  (func (export "mono") (param $n i32) (result i32)
    (local $acc i32) (local $i i32)
    (block $break
      (loop $cont
        (br_if $break (i32.ge_u (local.get $i) (local.get $n)))
        (local.set $acc (call_indirect (type $sig) (local.get $acc) (i32.const 0)))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $cont)))
    (local.get $acc))

  ;; polymorphic/megamorphic: slot cycles 0..7 with the counter.
  (func (export "poly") (param $n i32) (result i32)
    (local $acc i32) (local $i i32)
    (block $break
      (loop $cont
        (br_if $break (i32.ge_u (local.get $i) (local.get $n)))
        (local.set $acc (call_indirect (type $sig) (local.get $acc) (i32.and (local.get $i) (i32.const 7))))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $cont)))
    (local.get $acc))

  ;; monomorphic dispatch into a heavier method — measures dilution of the
  ;; fixed dispatch overhead as callee work grows. Slot 7 is $mh via a second
  ;; table so $mh is reachable indirectly.
  (func (export "mono_heavy") (param $n i32) (result i32)
    (local $acc i32) (local $i i32)
    (block $break
      (loop $cont
        (br_if $break (i32.ge_u (local.get $i) (local.get $n)))
        (local.set $acc (call_indirect (type $sig) (local.get $acc) (i32.const 8)))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $cont)))
    (local.get $acc))

  ;; direct call into the heavier method — floor for mono_heavy.
  (func (export "direct_heavy") (param $n i32) (result i32)
    (local $acc i32) (local $i i32)
    (block $break
      (loop $cont
        (br_if $break (i32.ge_u (local.get $i) (local.get $n)))
        (local.set $acc (call $mh (local.get $acc)))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $cont)))
    (local.get $acc))

  ;; direct-call floor: same callee $m0, no dispatch.
  (func (export "direct") (param $n i32) (result i32)
    (local $acc i32) (local $i i32)
    (block $break
      (loop $cont
        (br_if $break (i32.ge_u (local.get $i) (local.get $n)))
        (local.set $acc (call $m0 (local.get $acc)))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $cont)))
    (local.get $acc)))
