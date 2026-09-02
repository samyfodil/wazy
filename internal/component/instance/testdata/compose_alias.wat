;; A hand-written COMPOSED component in the exact shape `wasm-tools compose`
;; and `wac plug` emit: a provider is instantiated, one of its instance-typed
;; exports is projected out with `(alias export ... (instance))`, and THAT alias
;; -- not the provider itself -- is the instantiate-arg linking the consumer.
;;
;;   (instance $p (instantiate $Provider))
;;   (alias export $p "test:compose/adder@1.0.0" (instance $g))
;;   (instance $c (instantiate $Consumer (with "test:compose/adder@1.0.0" (instance $g))))
;;
;; The provider exports the interface the way wit-component does: a nested
;; "shim" component that RE-EXPORTS a resource type and the funcs it was handed
;; as instantiate-args, so the resource the consumer imports is reachable only
;; THROUGH the aliased export (the provider has no top-level type export at
;; all). That is what exportedInstanceResourceDefs' Kind 0x00 arm walks.
;;
;; run() returns scale(1) + value*1000 + dtorCount*10 + add(1,2)
;; == 100000 + 7*1000 + 1*10 + 3, so a single number proves every crossing at
;; once: an own<counter> minted by the provider and returned to the consumer, a
;; borrow<counter> sent back the other way, the consumer's own
;; `canon resource.drop` running the PROVIDER's destructor (the
;; resource-identity part), and a SECOND projected interface bound off the same
;; provider instance through its own alias.
(component
  (component $Provider
    ;; The destructor lives in its own core module so the resource type can
    ;; name it without a cycle (the main module needs resource.new/rep, which
    ;; need the type, which needs the dtor).
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
      ;; A borrow<counter> arriving at the resource's OWN implementing
      ;; instance is passed as the bare rep, not as a handle -- the canonical
      ;; ABI's same-instance exemption in lower_borrow (`if cx.inst is
      ;; t.rt.impl: return rep`), which is also why real wit-bindgen guest
      ;; code dereferences the param instead of calling resource.rep on it.
      (func (export "value") (param $rep i32) (result i32) (local.get $rep))
      (func (export "add") (param i32 i32) (result i32)
        (i32.add (local.get 0) (local.get 1)))
      (func (export "scale") (param i32) (result i32)
        (i32.mul (local.get 0) (i32.const 100000))))
    (core instance $maini (instantiate $main
      (with "res" (instance (export "new" (func $new))))))

    (func $make (param "v" u32) (result (own $counter)) (canon lift (core func $maini "make")))
    (func $value (param "c" (borrow $counter)) (result u32) (canon lift (core func $maini "value")))
    (func $add (param "a" u32) (param "b" u32) (result u32) (canon lift (core func $maini "add")))
    (func $count (result u32) (canon lift (core func $dtormi "count")))
    (func $scale (param "v" u32) (result u32) (canon lift (core func $maini "scale")))

    ;; The wit-component export shim: it IMPORTS the resource type and the
    ;; funcs, and re-exports them under the interface's member names.
    (component $shim
      (import "import-type-counter" (type $c (sub resource)))
      (import "make" (func $make (param "v" u32) (result (own $c))))
      (import "value" (func $value (param "c" (borrow $c)) (result u32)))
      (import "add" (func $add (param "a" u32) (param "b" u32) (result u32)))
      (import "dtor-count" (func $count (result u32)))
      ;; Each func export is re-ascribed against the EXPORTED resource type
      ;; $ec rather than the imported $c -- exactly what wit-component emits,
      ;; and what makes the instance valid to export at all.
      (export $ec "counter" (type $c))
      (export "make" (func $make) (func (param "v" u32) (result (own $ec))))
      (export "value" (func $value) (func (param "c" (borrow $ec)) (result u32)))
      (export "add" (func $add) (func (param "a" u32) (param "b" u32) (result u32)))
      (export "dtor-count" (func $count) (func (result u32))))
    (instance $iface (instantiate $shim
      (with "import-type-counter" (type $counter))
      (with "make" (func $make))
      (with "value" (func $value))
      (with "add" (func $add))
      (with "dtor-count" (func $count))))
    (export "test:compose/adder@1.0.0" (instance $iface))

    ;; A SECOND exported interface, so the consumer takes two projected args
    ;; off the same provider instance. Each arg has to resolve its own
    ;; projection: binding either one under the other's name would still
    ;; "work" for one of them and silently mis-wire the other. Interleaving it
    ;; after the first export also shifts the component instance index space,
    ;; which is what makes the alias slots below non-trivial.
    (instance $iface2 (export "scale" (func $scale)))
    (export "test:compose/second@1.0.0" (instance $iface2)))

  (component $Consumer
    (import "test:compose/adder@1.0.0" (instance $adder
      (export "counter" (type $c (sub resource)))
      (export "make" (func (param "v" u32) (result (own $c))))
      (export "value" (func (param "c" (borrow $c)) (result u32)))
      (export "add" (func (param "a" u32) (param "b" u32) (result u32)))
      (export "dtor-count" (func (result u32)))))
    (import "test:compose/second@1.0.0" (instance $second
      (export "scale" (func (param "v" u32) (result u32)))))
    (alias export $second "scale" (func $scale))
    (alias export $adder "counter" (type $counter))
    (alias export $adder "make" (func $make))
    (alias export $adder "value" (func $value))
    (alias export $adder "add" (func $add))
    (alias export $adder "dtor-count" (func $count))

    (core func $make' (canon lower (func $make)))
    (core func $value' (canon lower (func $value)))
    (core func $add' (canon lower (func $add)))
    (core func $count' (canon lower (func $count)))
    (core func $scale' (canon lower (func $scale)))
    (core func $drop' (canon resource.drop $counter))

    (core module $main
      (import "a" "make" (func $make (param i32) (result i32)))
      (import "a" "value" (func $value (param i32) (result i32)))
      (import "a" "add" (func $add (param i32 i32) (result i32)))
      (import "a" "dtor-count" (func $count (result i32)))
      (import "a" "drop" (func $drop (param i32)))
      (import "b" "scale" (func $scale (param i32) (result i32)))
      (func (export "run") (result i32)
        (local $h i32) (local $v i32)
        (local.set $h (call $make (i32.const 7)))
        (local.set $v (call $value (local.get $h)))
        ;; The consumer drops a handle to a resource the PROVIDER defines: the
        ;; provider's destructor has to run, through the composition-global
        ;; resource identity, or $count stays 0.
        (call $drop (local.get $h))
        (i32.add
          (call $scale (i32.const 1))
          (i32.add
            (i32.add (i32.mul (local.get $v) (i32.const 1000))
                     (i32.mul (call $count) (i32.const 10)))
            (call $add (i32.const 1) (i32.const 2))))))
    (core instance $maini (instantiate $main
      (with "a" (instance
        (export "make" (func $make'))
        (export "value" (func $value'))
        (export "add" (func $add'))
        (export "dtor-count" (func $count'))
        (export "drop" (func $drop'))))
      (with "b" (instance (export "scale" (func $scale'))))))

    (func $run (result u32) (canon lift (core func $maini "run")))
    (export "run" (func $run)))

  (instance $p (instantiate $Provider))
  (alias export $p "test:compose/adder@1.0.0" (instance $g))
  (alias export $p "test:compose/second@1.0.0" (instance $g2))
  (instance $c (instantiate $Consumer
    (with "test:compose/adder@1.0.0" (instance $g))
    (with "test:compose/second@1.0.0" (instance $g2))))
  (alias export $c "run" (func $run))
  (export "run" (func $run)))
