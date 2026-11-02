;; Throws the tag a host module exports as env.__cpp_exception.
(module
  (tag $e (import "env" "__cpp_exception") (param i32))
  (func (export "throw") (param i32)
    (throw $e (local.get 0)))
)
