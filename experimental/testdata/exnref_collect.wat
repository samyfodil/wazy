;; Exceptions are reclaimed once nothing holds their exnref, and kept while
;; something does. See TestExnRef_collected.
(module
  (tag $t (param i32))
  (func $throw (param i32) (throw $t (local.get 0)))

  ;; churn throws and catches by reference $n times, dropping every exnref.
  (func $churn (export "churn") (param $n i32)
    (loop $l
      (block $c (result i32 exnref)
        (try_table (catch_ref $t $c) (call $throw (local.get $n)))
        (unreachable))
      (drop)
      (drop)
      (br_if $l (local.tee $n (i32.sub (local.get $n) (i32.const 1))))))

  ;; hold keeps one exnref in a local through $n more catches in its own loop,
  ;; so the reference sits in a register across every loop-header safepoint,
  ;; then re-throws it: it must still be the $t carrying 42.
  (func (export "hold") (param $n i32) (result i32)
    (local $x exnref)
    (block $c (result i32 exnref)
      (try_table (catch_ref $t $c) (call $throw (i32.const 42)))
      (unreachable))
    (local.set $x)
    (drop)
    (loop $l
      (block $c (result i32 exnref)
        (try_table (catch_ref $t $c) (call $throw (local.get $n)))
        (unreachable))
      (drop)
      (drop)
      (br_if $l (local.tee $n (i32.sub (local.get $n) (i32.const 1)))))
    (block $c (result i32)
      (try_table (catch $t $c) (throw_ref (local.get $x)))
      (unreachable))))
