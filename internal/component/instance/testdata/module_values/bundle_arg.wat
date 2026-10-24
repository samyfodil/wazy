;; An inline bundle passed as an instance arg to a nested component with code
;; of its own: the nested component instantiates the module the bundle
;; carries, with the func the bundle carries -- a sibling's export -- lowered
;; as that module's import. The module's start traps unless the call returns
;; 42.
(component
  (component $provider
    (core module $impl (func (export "f") (result i32) i32.const 42))
    (core instance $impl (instantiate $impl))
    (func (export "f") (result u32) (canon lift (core func $impl "f")))
  )
  (instance $p (instantiate $provider))
  (alias export $p "f" (func $f))

  (core module $verify
    (import "" "f" (func $f (result i32)))
    (func $start
      call $f
      i32.const 42
      i32.ne
      if unreachable end)
    (start $start)
  )
  (instance $b
    (export "m" (core module $verify))
    (export "f" (func $f))
  )

  (component $c
    (import "b" (instance $b
      (export "m" (core module (import "" "f" (func (result i32)))))
      (export "f" (func (result u32)))
    ))
    (alias export $b "f" (func $bf))
    (core func $lf (canon lower (func $bf)))
    (core instance $fs (export "f" (func $lf)))
    (core instance (instantiate (module $b "m") (with "" (instance $fs))))
  )
  (instance (instantiate $c (with "b" (instance $b))))
)
