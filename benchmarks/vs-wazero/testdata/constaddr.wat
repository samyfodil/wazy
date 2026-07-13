;; constaddr.wat — a hot loop of constant-address i32 loads, the workload that
;; exercises wazero PR #2514 / wazy C21 (bounds-check elision for constant
;; addresses provably within the memory minimum). Every load is at an i32.const
;; address well inside the 1-page (65536-byte) minimum, so all bounds checks
;; can be elided. Mirrors the interpreter-style pattern of state living in
;; globals/memory at fixed addresses inside a loop.
;;
;; Regenerate: wat2wasm testdata/constaddr.wat -o testdata/constaddr.wasm
(module
  (memory 1)
  (func (export "work") (param $n i32) (result i32)
    (local $acc i32) (local $i i32)
    (block $break
      (loop $cont
        (br_if $break (i32.ge_u (local.get $i) (local.get $n)))
        (local.set $acc
          (i32.add (local.get $acc)
            (i32.add
              (i32.add
                (i32.add (i32.load (i32.const 0))  (i32.load (i32.const 4)))
                (i32.add (i32.load (i32.const 8))  (i32.load (i32.const 12))))
              (i32.add
                (i32.add (i32.load (i32.const 16)) (i32.load (i32.const 20)))
                (i32.add (i32.load (i32.const 24)) (i32.load (i32.const 28)))))))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $cont)))
    (local.get $acc)))
