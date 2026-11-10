;; `use types.{error-code}` in WIT, as wit-component encodes it inside an imported instance type,
;; feeding future<result<_, error-code>>: the shape of every WASI 0.3 stream (issue #102).
(component
  (type $types-ty (instance
    (type $e (enum "io" "pipe"))
    (export "error-code" (type (eq $e)))))
  (import "test:repro/types" (instance $types (type $types-ty)))
  (alias export $types "error-code" (type $error-code))

  (type $user-ty (instance
    (alias outer 1 $error-code (type $ec))
    (export "error-code" (type (eq $ec)))))
  (import "test:repro/user" (instance $user (type $user-ty)))
  (alias export $user "error-code" (type $user-error-code))

  (type $res (result (error $user-error-code)))
  (type $fut (future $res))
  (core func $drop (canon future.drop-readable $fut))

  (core module $m (import "" "drop" (func (param i32))))
  (core instance (instantiate $m
    (with "" (instance (export "drop" (func $drop))))))
)
