;; An active element segment on a 64-bit table whose offset comes from an
;; imported i64 global. With reference types disabled the segment's bounds are
;; checked at instantiation rather than at validation, and that check has to see
;; the whole 64-bit offset: narrowing it to 32 bits turns 2^32 into 0, which fits.
(module
  (import "env" "offset" (global $offset i64))
  (table i64 1 funcref)
  (func $f)
  (elem (global.get $offset) func $f)
)
