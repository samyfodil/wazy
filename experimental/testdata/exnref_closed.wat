;; A module whose code never holds an exnref: it throws and catches by value
;; only, and makes no indirect calls, so a call into it registers with the
;; collector at its first throw rather than on entry. See
;; TestExnRef_closedModuleRegistersAtFirstThrow.
(module
  (tag $t (param i32))
  (func $throw (param i32) (throw $t (local.get 0)))

  ;; churn throws $n, $n-1, ..., 1 and catches each, returning their sum.
  (func (export "churn") (param $n i32) (result i32) (local $s i32)
    (loop $l
      (block $c (result i32)
        (try_table (catch $t $c) (call $throw (local.get $n)))
        (unreachable))
      (local.set $s (i32.add (local.get $s)))
      (br_if $l (local.tee $n (i32.sub (local.get $n) (i32.const 1)))))
    (local.get $s)))
