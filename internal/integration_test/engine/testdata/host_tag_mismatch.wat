;; Imports env.__cpp_exception with an i64 parameter where the host defines i32.
(module
  (tag (import "env" "__cpp_exception") (param i64))
)
