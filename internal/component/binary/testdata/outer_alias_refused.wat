;; Outer aliases the component model's validator rejects, which the decoder must refuse rather
;; than hand back an index of the wrong component's space: a handle (first child's type 0), an
;; index past the enclosing space (its type 2), and a handle through an imported instance type
;; (second child's type 1). A resource passes through as the tag it is (first child's type 1).
;; Not valid: build with wasm-tools parse.
(component
  (type $res (resource (rep i32)))
  (type $h (own $res))
  (type $rec (record (field "h" $h)))
  (component
    (alias outer 1 $rec (type))
    (alias outer 1 $res (type))
    (alias outer 1 99 (type)))
  (component
    (type $user-ty (instance
      (alias outer 2 $rec (type $r))
      (export "r" (type (eq $r)))))
    (import "test:outer/user" (instance $user (type $user-ty)))
    (alias export $user "r" (type)))
)
