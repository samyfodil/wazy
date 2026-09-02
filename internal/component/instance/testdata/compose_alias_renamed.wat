;; The same composition as compose_alias.wat, deliberately bent in the three
;; ways the binary format allows but no composer happens to produce:
;;
;;   1. The PROVIDER exports the interface as "other:pkg/iface@9.9.99" while
;;      the CONSUMER still imports "test:compose/adder@1.0.0". The alias names
;;      the provider's export name X, the instantiate-arg names the importee's
;;      import name Y, and X != Y. Subtyping is checked on the instance TYPE --
;;      its member names and types -- never on the outer name, so this is
;;      valid; resolving the alias to "the provider itself" and letting name
;;      matching run would bind nothing at all here.
;;   2. The provider builds the exported interface with the INLINE-EXPORT
;;      instance form (`(instance (export ...) ...)`, Kind 0x01) rather than by
;;      instantiating a shim, exercising the other arm of
;;      exportedInstanceResourceDefs.
;;   3. The outer component re-exports the CONSUMER's interface through an
;;      instance alias -- `(alias export $c "..." (instance $r))` +
;;      `(export "..." (instance $r))` -- the shape composed componentize-py
;;      output uses, where the export names an alias index rather than an
;;      instance definition.
;;
;; run() returns the same 7*1000 + 1*10 + 3 == 7013 as compose_alias.wat.
(component
  (component $Provider
    (core module $dtorm
      (global $count (mut i32) (i32.const 0))
      (func (export "dtor") (param i32)
        (global.set $count (i32.add (global.get $count) (i32.const 1))))
      (func (export "count") (result i32) (global.get $count)))
    (core instance $dtormi (instantiate $dtorm))

    (type $counter (resource (rep i32) (dtor (func $dtormi "dtor"))))
    (core func $new (canon resource.new $counter))

    (core module $main
      (import "res" "new" (func $new (param i32) (result i32)))
      (func (export "make") (param $v i32) (result i32) (call $new (local.get $v)))
      ;; Same-instance borrow exemption: the param is the bare rep. See
      ;; compose_alias.wat.
      (func (export "value") (param $rep i32) (result i32) (local.get $rep))
      (func (export "add") (param i32 i32) (result i32)
        (i32.add (local.get 0) (local.get 1))))
    (core instance $maini (instantiate $main
      (with "res" (instance (export "new" (func $new))))))

    (func $make (param "v" u32) (result (own $counter)) (canon lift (core func $maini "make")))
    (func $value (param "c" (borrow $counter)) (result u32) (canon lift (core func $maini "value")))
    (func $add (param "a" u32) (param "b" u32) (result u32) (canon lift (core func $maini "add")))
    (func $count (result u32) (canon lift (core func $dtormi "count")))

    (instance $iface
      (export "counter" (type $counter))
      (export "make" (func $make))
      (export "value" (func $value))
      (export "add" (func $add))
      (export "dtor-count" (func $count)))
    (export "other:pkg/iface@9.9.99" (instance $iface)))

  (component $Consumer
    (import "test:compose/adder@1.0.0" (instance $adder
      (export "counter" (type $c (sub resource)))
      (export "make" (func (param "v" u32) (result (own $c))))
      (export "value" (func (param "c" (borrow $c)) (result u32)))
      (export "add" (func (param "a" u32) (param "b" u32) (result u32)))
      (export "dtor-count" (func (result u32)))))
    (alias export $adder "counter" (type $counter))
    (alias export $adder "make" (func $make))
    (alias export $adder "value" (func $value))
    (alias export $adder "add" (func $add))
    (alias export $adder "dtor-count" (func $count))

    (core func $make' (canon lower (func $make)))
    (core func $value' (canon lower (func $value)))
    (core func $add' (canon lower (func $add)))
    (core func $count' (canon lower (func $count)))
    (core func $drop' (canon resource.drop $counter))

    (core module $main
      (import "a" "make" (func $make (param i32) (result i32)))
      (import "a" "value" (func $value (param i32) (result i32)))
      (import "a" "add" (func $add (param i32 i32) (result i32)))
      (import "a" "dtor-count" (func $count (result i32)))
      (import "a" "drop" (func $drop (param i32)))
      (func (export "run") (result i32)
        (local $h i32) (local $v i32)
        (local.set $h (call $make (i32.const 7)))
        (local.set $v (call $value (local.get $h)))
        (call $drop (local.get $h))
        (i32.add
          (i32.add (i32.mul (local.get $v) (i32.const 1000))
                   (i32.mul (call $count) (i32.const 10)))
          (call $add (i32.const 1) (i32.const 2)))))
    (core instance $maini (instantiate $main
      (with "a" (instance
        (export "make" (func $make'))
        (export "value" (func $value'))
        (export "add" (func $add'))
        (export "dtor-count" (func $count'))
        (export "drop" (func $drop'))))))

    (func $run (result u32) (canon lift (core func $maini "run")))
    (instance $runner (export "run" (func $run)))
    (export "test:compose/runner@1.0.0" (instance $runner)))

  (instance $p (instantiate $Provider))
  (alias export $p "other:pkg/iface@9.9.99" (instance $g))
  (instance $c (instantiate $Consumer (with "test:compose/adder@1.0.0" (instance $g))))
  (alias export $c "test:compose/runner@1.0.0" (instance $r))
  (export "test:compose/runner@1.0.0" (instance $r)))
