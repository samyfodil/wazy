;; An exnref kept in a global between calls, then held only in a local of a
;; call that allocates nothing while other calls collect. See
;; TestExnRef_heldByImportedFunction.
(module
  (tag $t (param i32))
  (global $g (mut exnref) (ref.null exn))
  (func $throw (param i32) (throw $t (local.get 0)))

  ;; stash keeps an exnref for a $t carrying 42 in $g.
  (func (export "stash")
    (block $c (result i32 exnref)
      (try_table (catch_ref $t $c) (call $throw (i32.const 42)))
      (unreachable))
    (global.set $g)
    (drop))

  ;; churn throws and catches by reference $n times, dropping every exnref.
  (func (export "churn") (param $n i32)
    (loop $l
      (block $c (result i32 exnref)
        (try_table (catch_ref $t $c) (call $throw (local.get $n)))
        (unreachable))
      (drop)
      (drop)
      (br_if $l (local.tee $n (i32.sub (local.get $n) (i32.const 1))))))

  ;; check moves the stashed exnref into a local, clears $g, spins $spin times
  ;; without allocating, then re-throws it: it must still be the $t carrying 42.
  (func (export "check") (param $spin i32) (result i32) (local $x exnref)
    (local.set $x (global.get $g))
    (global.set $g (ref.null exn))
    (loop $l
      (br_if $l (local.tee $spin (i32.sub (local.get $spin) (i32.const 1)))))
    (block $c (result i32)
      (try_table (catch $t $c) (throw_ref (local.get $x)))
      (unreachable))))
