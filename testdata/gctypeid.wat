;; gctypeid.wat -- a rec group with a supertype and a subtype, and two ref.test
;; results over them. Used by TestInstantiateModule_typeIDsAreThisStores_gc to
;; catch a stale store-specific type ID, which ref.test turns into a silently
;; wrong boolean rather than an error.
;;
;; Rebuild with: wasm-tools parse testdata/gctypeid.wat -o testdata/gctypeid.wasm
;; (wabt's wat2wasm cannot assemble this -- it has no `rec` support.)
(module
  (rec
    (type $a (sub (struct (field i32))))
    (type $b (sub $a (struct (field i32) (field i64)))))
  (func (export "b_is_a") (result i32)
    (ref.test (ref $a) (struct.new $b (i32.const 1) (i64.const 2))))
  (func (export "a_is_b") (result i32)
    (ref.test (ref $b) (struct.new $a (i32.const 1)))))
