;; A FUNC alias whose instance operand is itself an INSTANCE ALIAS.
;;
;;   (instance $p (instantiate $Provider))                        ;; a DEFINITION
;;   (alias export $p "test:compose/greeter@1.0.0" (instance $g))  ;; an ALIAS
;;   (alias export $g "greet" (func $greet))                       ;; off the ALIAS
;;   (export "greet" (func $greet))
;;
;; Every producer of the component-instance index space is a legal operand for a
;; func alias -- Explainer.md's alias section says an instance alias "introduces
;; an index denoting an instance-typed export of an instance", full stop, with
;; nothing making that index second-class. But only ONE of the four producers is
;; an instance DEFINITION, and a definition is the only kind
;; instantiateNestedInstances has a sub-Instance filed under. So a func alias
;; naming $g above misses a lookup keyed by definition slots, and the export
;; used to be rejected with "resolves to an imported func rather than a lift;
;; only lifted funcs may be exported" -- a diagnostic stating the opposite of
;; what happened, since $greet is neither imported nor anything but a lift: it
;; is a perfectly good `canon lift` one level of aliasing away.
;;
;; The counts are the minimum that can express the shape: the provider must
;; export an INSTANCE (so there is something instance-typed to alias out) and
;; that instance must carry a FUNC member (so there is something func-typed to
;; alias out of the alias). No resource, no consumer and no second interface are
;; needed. The `(export "greet" (func $greet))` at the end is load-bearing --
;; drop it and the alias is never bound, so nothing fails. The param is a plain
;; u32 for the same reason: the defect has nothing to do with the signature.
;;
;; The interface is spelled with the inline-export instance form (Kind 0x01)
;; rather than wit-component's instantiate-a-shim form (Kind 0x00, which
;; compose_alias.wat pins) purely to keep the fixture short; the two resolve
;; through the same path from here on.
;;
;; greet(41) == 42.
(component
  (component $Provider
    (core module $main
      (func (export "greet") (param i32) (result i32)
        (i32.add (local.get 0) (i32.const 1))))
    (core instance $maini (instantiate $main))
    (func $greet (param "n" u32) (result u32) (canon lift (core func $maini "greet")))
    (instance $iface (export "greet" (func $greet)))
    (export "test:compose/greeter@1.0.0" (instance $iface)))

  (instance $p (instantiate $Provider))
  (alias export $p "test:compose/greeter@1.0.0" (instance $g))
  (alias export $g "greet" (func $greet))
  (export "greet" (func $greet)))
