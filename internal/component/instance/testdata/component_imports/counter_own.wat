;; Not of the type counter_root.wat declares: its method takes the counter
;; by own, where the declared one borrows it.
(component
  (type $c (resource (rep i32)))
  (core func $new (canon resource.new $c))
  (core instance $imp (export "new" (func $new)))
  (core module $m
    (import "" "new" (func $new (param i32) (result i32)))
    (func (export "ctor") (param i32) (result i32) local.get 0 call $new)
    (func (export "get") (param i32) (result i32) local.get 0))
  (core instance $i (instantiate $m (with "" (instance $imp))))
  (export $ce "counter" (type $c))
  (func (export "[constructor]counter") (param "v" u32) (result (own $ce)) (canon lift (core func $i "ctor")))
  (func (export "[method]counter.get") (param "self" (own $ce)) (result u32) (canon lift (core func $i "get")))
)
