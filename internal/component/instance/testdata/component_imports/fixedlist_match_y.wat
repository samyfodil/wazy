;; A component of the type fixedlist_match_root.wat imports as `y`.
(component
  (type $t (list s32 10))
  (import "x" (type (eq $t)))
)
