;; compose_func_alias.wat's alias-off-an-alias, put to the OTHER use a func
;; index has: a `(with "greet" (func $greet))` instantiate-arg feeding a sibling
;; nested component, alongside the plain export.
;;
;;   (instance $p (instantiate $Provider))
;;   (alias export $p "test:compose/greeter@1.0.0" (instance $g))
;;   (alias export $g "greet" (func $greet))
;;   (instance $c (instantiate $Consumer (with "greet" (func $greet))))   ;; <- here
;;   (export "greet" (func $greet))                                       ;; <- and here
;;
;; Same root cause, a second call site: outerFuncArgImport resolved the alias's
;; instance operand the same definition-slots-only way the export side did, and
;; failed on an alias-produced slot with "component instance index 1 out of
;; range of 0 imported instances" -- it had concluded the operand must name an
;; IMPORTED instance, having found it was not a definition. Both sites now share
;; one resolution (resolveFuncAliasInstance), and this fixture is what keeps
;; them shared: the arg site is bound during instantiateNestedInstances, strictly
;; before any export is, so a component using the alias both ways exercises the
;; arg site first and the export site after.
;;
;; run() calls greet(40) through the lowered import == 41; greet(41) == 42.
(component
  (component $Provider
    (core module $main
      (func (export "greet") (param i32) (result i32)
        (i32.add (local.get 0) (i32.const 1))))
    (core instance $maini (instantiate $main))
    (func $greet (param "n" u32) (result u32) (canon lift (core func $maini "greet")))
    (instance $iface (export "greet" (func $greet)))
    (export "test:compose/greeter@1.0.0" (instance $iface)))

  (component $Consumer
    (import "greet" (func $greet (param "n" u32) (result u32)))
    (core func $greet' (canon lower (func $greet)))
    (core module $main
      (import "a" "greet" (func $greet (param i32) (result i32)))
      (func (export "run") (result i32) (call $greet (i32.const 40))))
    (core instance $maini (instantiate $main
      (with "a" (instance (export "greet" (func $greet'))))))
    (func $run (result u32) (canon lift (core func $maini "run")))
    (export "run" (func $run)))

  (instance $p (instantiate $Provider))
  (alias export $p "test:compose/greeter@1.0.0" (instance $g))
  (alias export $g "greet" (func $greet))
  (instance $c (instantiate $Consumer (with "greet" (func $greet))))
  (alias export $c "run" (func $run))
  (export "greet" (func $greet))
  (export "run" (func $run)))
