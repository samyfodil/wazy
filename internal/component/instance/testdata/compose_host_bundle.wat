;; A host resource and the host funcs using it, passed on to a nested
;; component in an inline bundle and in an instance of a pure re-export shim.
;; $C makes a thing through the host, reads it back through a borrow and
;; drops it, which runs the host destructor; run returns what peek did.
(component
  (import "acme:api/host" (instance $h
    (export "thing" (type $thing (sub resource)))
    (export "make" (func (param "v" u32) (result (own $thing))))
    (export "peek" (func (param "x" (borrow $thing)) (result u32)))))
  (alias export $h "thing" (type $thing))
  (alias export $h "make" (func $make))
  (alias export $h "peek" (func $peek))

  (component $C
    (import "host" (instance $h
      (export "thing" (type $thing (sub resource)))
      (export "make" (func (param "v" u32) (result (own $thing))))
      (export "peek" (func (param "x" (borrow $thing)) (result u32)))))
    (alias export $h "thing" (type $thing))
    (alias export $h "make" (func $make))
    (alias export $h "peek" (func $peek))
    (core func $make' (canon lower (func $make)))
    (core func $peek' (canon lower (func $peek)))
    (core func $drop (canon resource.drop $thing))
    (core module $m
      (import "h" "make" (func $make (param i32) (result i32)))
      (import "h" "peek" (func $peek (param i32) (result i32)))
      (import "h" "drop" (func $drop (param i32)))
      (func (export "run") (result i32)
        (local $h i32) (local $v i32)
        (local.set $h (call $make (i32.const 9)))
        (local.set $v (call $peek (local.get $h)))
        (call $drop (local.get $h))
        (local.get $v)))
    (core instance $i (instantiate $m
      (with "h" (instance
        (export "make" (func $make'))
        (export "peek" (func $peek'))
        (export "drop" (func $drop))))))
    (func (export "run") (result u32) (canon lift (core func $i "run"))))

  (component $S
    (import "thing" (type $thing (sub resource)))
    (import "make" (func $make (param "v" u32) (result (own $thing))))
    (import "peek" (func $peek (param "x" (borrow $thing)) (result u32)))
    (export "thing" (type $thing))
    (export "make" (func $make))
    (export "peek" (func $peek)))

  (instance $b
    (export "thing" (type $thing))
    (export "make" (func $make))
    (export "peek" (func $peek)))
  (instance $s (instantiate $S
    (with "thing" (type $thing))
    (with "make" (func $make))
    (with "peek" (func $peek))))
  (instance $cb (instantiate $C (with "host" (instance $b))))
  (instance $cs (instantiate $C (with "host" (instance $s))))
  (export "bundle-run" (func $cb "run"))
  (export "shim-run" (func $cs "run")))
