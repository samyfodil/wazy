;; Catches env.__cpp_exception thrown by another module: it can only do so if
;; both modules import the very same tag.
(module
  (import "thrower" "throw" (func $throw (param i32)))
  (tag $e (import "env" "__cpp_exception") (param i32))
  (func (export "catch") (param i32) (result i32)
    (block $h (result i32)
      (try_table (catch $e $h)
        (call $throw (local.get 0)))
      (i32.const -1)))
)
