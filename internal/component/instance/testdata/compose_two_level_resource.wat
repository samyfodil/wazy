;; compose_two_level.wat's shape again, now carrying a RESOURCE with a
;; destructor across both composition levels -- and renaming the interface at
;; every hop, because nothing in the binary format ties the three names
;; together.
;;
;;   $Provider  defines  "test:compose/counter@1.0.0"   (the owner's own name)
;;   $Wrapper   projects that and republishes it as
;;                       "vendor:wrapped/counter@2.0.0"
;;   the outer  projects THAT and passes it to $Consumer under
;;                       "test:compose/counter@1.0.0"   (the importee's name)
;;
;; Ownership is what has to survive all of that. An own<counter> is minted by
;; $Provider, handed to $Consumer two levels up, dropped there through
;; $Consumer's OWN `canon resource.drop` on its OWN local resource index, and
;; that drop has to run $PROVIDER's destructor. $Wrapper has no part in it: it
;; never composed the resource into itself, has no tag, no destructor and no
;; TypeSpace entry for it, and lining the interface up against $Wrapper leaves
;; every crossing handle untagged. So the resource line-up has to be resolved
;; against the OWNER, found through Instance.reexposedInstances, under the
;; OWNER's OWN name for the interface -- the middle name above is a dead end,
;; and using it (rather than chasing through to the first) silently yields no
;; resources at all rather than an error.
;;
;; $Provider exports the interface the wit-component way -- a nested "shim"
;; component re-exporting a resource type and the funcs it was handed as
;; instantiate-args -- so the walk under test is exportedInstanceResourceDefs'
;; Kind 0x00 arm, run against the owner two levels down.
;;
;; run() returns value*10 + dtorCount == 7*10 + 1 == 71: the 70 says the
;; own<counter> and the borrow<counter> crossed both levels with their rep
;; intact, the 1 says the consumer's drop found the provider's destructor.
(component
  (component $Wrapper
    (component $Provider
      ;; The destructor lives in its own core module so the resource type can
      ;; name it without a cycle (the main module needs resource.new, which
      ;; needs the type, which needs the dtor).
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
        ;; ABI's same-instance exemption in lower_borrow.
        (func (export "value") (param $rep i32) (result i32) (local.get $rep)))
      (core instance $maini (instantiate $main
        (with "res" (instance (export "new" (func $new))))))

      (func $make (param "v" u32) (result (own $counter)) (canon lift (core func $maini "make")))
      (func $value (param "c" (borrow $counter)) (result u32) (canon lift (core func $maini "value")))
      (func $count (result u32) (canon lift (core func $dtormi "count")))

      (component $shim
        (import "import-type-counter" (type $c (sub resource)))
        (import "make" (func $make (param "v" u32) (result (own $c))))
        (import "value" (func $value (param "c" (borrow $c)) (result u32)))
        (import "dtor-count" (func $count (result u32)))
        (export $ec "counter" (type $c))
        (export "make" (func $make) (func (param "v" u32) (result (own $ec))))
        (export "value" (func $value) (func (param "c" (borrow $ec)) (result u32)))
        (export "dtor-count" (func $count) (func (result u32))))
      (instance $iface (instantiate $shim
        (with "import-type-counter" (type $counter))
        (with "make" (func $make))
        (with "value" (func $value))
        (with "dtor-count" (func $count))))
      (export "test:compose/counter@1.0.0" (instance $iface)))

    (instance $p (instantiate $Provider))
    (alias export $p "test:compose/counter@1.0.0" (instance $g))
    ;; Republished under a DIFFERENT name: level 2 can only reach the owner's
    ;; own name by chasing the record, never by reusing the one it sees here.
    (export "vendor:wrapped/counter@2.0.0" (instance $g)))

  (component $Consumer
    (import "test:compose/counter@1.0.0" (instance $ctr
      (export "counter" (type $c (sub resource)))
      (export "make" (func (param "v" u32) (result (own $c))))
      (export "value" (func (param "c" (borrow $c)) (result u32)))
      (export "dtor-count" (func (result u32)))))
    (alias export $ctr "counter" (type $counter))
    (alias export $ctr "make" (func $make))
    (alias export $ctr "value" (func $value))
    (alias export $ctr "dtor-count" (func $count))

    (core func $make' (canon lower (func $make)))
    (core func $value' (canon lower (func $value)))
    (core func $count' (canon lower (func $count)))
    (core func $drop' (canon resource.drop $counter))

    (core module $main
      (import "a" "make" (func $make (param i32) (result i32)))
      (import "a" "value" (func $value (param i32) (result i32)))
      (import "a" "dtor-count" (func $count (result i32)))
      (import "a" "drop" (func $drop (param i32)))
      (func (export "run") (result i32)
        (local $h i32) (local $v i32)
        (local.set $h (call $make (i32.const 7)))
        (local.set $v (call $value (local.get $h)))
        (call $drop (local.get $h))
        (i32.add (i32.mul (local.get $v) (i32.const 10)) (call $count))))
    (core instance $maini (instantiate $main
      (with "a" (instance
        (export "make" (func $make'))
        (export "value" (func $value'))
        (export "dtor-count" (func $count'))
        (export "drop" (func $drop'))))))

    (func $run (result u32) (canon lift (core func $maini "run")))
    (export "run" (func $run)))

  (instance $w (instantiate $Wrapper))
  (alias export $w "vendor:wrapped/counter@2.0.0" (instance $g2))
  (instance $c (instantiate $Consumer (with "test:compose/counter@1.0.0" (instance $g2))))
  (alias export $c "run" (func $run))
  (export "run" (func $run)))
