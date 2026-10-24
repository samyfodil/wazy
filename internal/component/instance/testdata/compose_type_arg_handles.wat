;; A resource and the funcs using it passed to a nested component as separate
;; type and func instantiate-args, not as an instance: $C makes an r through
;; $P, reads it back through a borrow, and drops it, which runs $P's
;; destructor. run returns value * 100 + drops = 501. run-bundle and run-shim
;; pass the same type aliased back out of an inline bundle and out of a shim
;; instance, the second after the funcs whose handles it types.
(component
  (component $P
    (core module $dtorm
      (global $count (mut i32) (i32.const 0))
      (func (export "dtor") (param i32)
        (global.set $count (i32.add (global.get $count) (i32.const 1))))
      (func (export "count") (result i32) (global.get $count)))
    (core instance $dtormi (instantiate $dtorm))
    (type $r (resource (rep i32) (dtor (core func $dtormi "dtor"))))
    (core func $new (canon resource.new $r))
    (core module $main
      (import "res" "new" (func $new (param i32) (result i32)))
      (func (export "make") (param i32) (result i32) (call $new (local.get 0)))
      (func (export "value") (param i32) (result i32) (local.get 0)))
    (core instance $maini (instantiate $main (with "res" (instance (export "new" (func $new))))))
    (export $r' "r" (type $r))
    (func (export "make") (param "v" u32) (result (own $r')) (canon lift (core func $maini "make")))
    (func (export "value") (param "x" (borrow $r')) (result u32) (canon lift (core func $maini "value")))
    (func (export "count") (result u32) (canon lift (core func $dtormi "count"))))

  (component $C
    (import "r" (type $r (sub resource)))
    (import "make" (func $make (param "v" u32) (result (own $r))))
    (import "value" (func $value (param "x" (borrow $r)) (result u32)))
    (import "count" (func $count (result u32)))
    (core func $make' (canon lower (func $make)))
    (core func $value' (canon lower (func $value)))
    (core func $count' (canon lower (func $count)))
    (core func $drop (canon resource.drop $r))
    (core module $main
      (import "p" "make" (func $make (param i32) (result i32)))
      (import "p" "value" (func $value (param i32) (result i32)))
      (import "p" "count" (func $count (result i32)))
      (import "p" "drop" (func $drop (param i32)))
      (func (export "run") (result i32)
        (local $h i32) (local $v i32)
        (local.set $h (call $make (i32.const 5)))
        (local.set $v (call $value (local.get $h)))
        (call $drop (local.get $h))
        (i32.add (i32.mul (local.get $v) (i32.const 100)) (call $count))))
    (core instance $maini (instantiate $main
      (with "p" (instance
        (export "make" (func $make'))
        (export "value" (func $value'))
        (export "count" (func $count'))
        (export "drop" (func $drop))))))
    (func (export "run") (result u32) (canon lift (core func $maini "run"))))

  (component $S
    (import "r" (type $r (sub resource)))
    (export "r" (type $r)))

  (instance $p (instantiate $P))
  (alias export $p "r" (type $r))
  (alias export $p "make" (func $make))
  (alias export $p "value" (func $value))
  (alias export $p "count" (func $count))
  (instance $c (instantiate $C
    (with "r" (type $r))
    (with "make" (func $make))
    (with "value" (func $value))
    (with "count" (func $count))))
  (export "run" (func $c "run"))
  (instance $b (export "r" (type $r)))
  (alias export $b "r" (type $rb))
  (instance $s (instantiate $S (with "r" (type $r))))
  (alias export $s "r" (type $rs))
  (instance $cb (instantiate $C
    (with "r" (type $rb))
    (with "make" (func $make))
    (with "value" (func $value))
    (with "count" (func $count))))
  (instance $cs (instantiate $C
    (with "count" (func $count))
    (with "value" (func $value))
    (with "make" (func $make))
    (with "r" (type $rs))))
  (export "run-bundle" (func $cb "run"))
  (export "run-shim" (func $cs "run")))
