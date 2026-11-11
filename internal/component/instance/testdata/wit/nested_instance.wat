;; An interface exporting an instance, which WIT has no word for.
(component
  (import "test:n/outer" (instance (export "inner" (instance)))))
