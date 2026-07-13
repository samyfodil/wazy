;; Test module for runtime interrupt-check interval retuning.
;;   spin(n)  : finite countdown loop, returns n (a loop with interrupt checks).
;;   forever(): infinite loop; only terminates via the module-exit-code check,
;;              so it exercises that the retuned yield still fires.
;; Regenerate: wat2wasm experimental/testdata/interrupt.wat -o experimental/testdata/interrupt.wasm
(module
  (func (export "spin") (param $n i64) (result i64)
    (local $i i64)
    (block $break
      (loop $cont
        (br_if $break (i64.ge_u (local.get $i) (local.get $n)))
        (local.set $i (i64.add (local.get $i) (i64.const 1)))
        (br $cont)))
    (local.get $i))
  (func (export "forever")
    (loop $cont (br $cont))))
