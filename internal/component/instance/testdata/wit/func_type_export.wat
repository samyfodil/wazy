;; An interface exporting a func type, which WIT has no word for.
(component
  (import "test:n/types" (instance (type (func)) (export "t" (type (eq 0))))))
