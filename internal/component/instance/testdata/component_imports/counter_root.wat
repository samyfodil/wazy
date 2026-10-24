;; A root component importing a component that defines a resource, and
;; re-exporting its instance whole: the resource, its constructor and its
;; method all reach the host through the supplied component.
(component
  (import "counter" (component $C
    (export "counter" (type $ct (sub resource)))
    (export "[constructor]counter" (func (param "v" u32) (result (own $ct))))
    (export "[method]counter.get" (func (param "self" (borrow $ct)) (result u32)))
  ))
  (instance $c (instantiate $C))
  (export "c" (instance $c))
)
