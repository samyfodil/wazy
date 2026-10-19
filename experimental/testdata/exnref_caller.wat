;; Calls exnref_stash.wat's check. Its own code never touches an exnref, but
;; the imported function's frames run in its calls. See
;; TestExnRef_heldByImportedFunction.
(module
  (import "stash" "check" (func $check (param i32) (result i32)))
  (func (export "check") (param i32) (result i32) (call $check (local.get 0))))
