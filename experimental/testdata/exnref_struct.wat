;; A struct that only an exception's params name, while only an exnref names
;; the exception, must survive collections. See TestExnRef_keepsParams.
(module
  (type $s (struct (field i32)))
  (tag $t (param (ref $s)))
  (func (export "run") (param $n i32) (result i32)
    (local $x exnref)
    (block $c (result (ref $s) exnref)
      (try_table (catch_ref $t $c) (throw $t (struct.new $s (i32.const 7))))
      (unreachable))
    (local.set $x)
    (drop)
    ;; Allocate enough garbage to force collections.
    (loop $l
      (drop (struct.new $s (i32.const 0)))
      (br_if $l (local.tee $n (i32.sub (local.get $n) (i32.const 1)))))
    (block $c (result (ref $s))
      (try_table (catch $t $c) (throw_ref (local.get $x)))
      (unreachable))
    (struct.get $s 0)))
