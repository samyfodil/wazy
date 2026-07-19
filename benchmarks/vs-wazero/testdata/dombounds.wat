;; dombounds.wat — a hot loop where redundant PHIs hide already-checked
;; addresses from the frontend bounds analysis. The 72-byte check dominates
;; all subsequent loads, but their address flows through an if/else merge.
;;
;; Regenerate: wat2wasm testdata/dombounds.wat -o testdata/dombounds.wasm
(module
  (memory 1)
  (func (export "work") (param $n i32) (param $base i32) (result i32)
    (local $i i32) (local $addr i32) (local $acc i32)
    (local.set $addr (local.get $base))
    (block $break
      (loop $continue
        (br_if $break (i32.ge_u (local.get $i) (local.get $n)))

        ;; This access proves [addr, addr+72) is in bounds.
        (drop (i64.load offset=64 (local.get $addr)))

        ;; Both definitions are identical, but the following block initially
        ;; sees a PHI until the redundant-PHI pass resolves it.
        (if (i32.and (local.get $i) (i32.const 1))
          (then (local.set $addr (local.get $addr)))
          (else (local.set $addr (local.get $addr))))

        (local.set $acc
          (i32.add (local.get $acc)
            (i32.add
              (i32.add
                (i32.add (i32.load offset=0 (local.get $addr))
                         (i32.load offset=8 (local.get $addr)))
                (i32.add (i32.load offset=16 (local.get $addr))
                         (i32.load offset=24 (local.get $addr))))
              (i32.add
                (i32.add (i32.load offset=32 (local.get $addr))
                         (i32.load offset=40 (local.get $addr)))
                (i32.add (i32.load offset=48 (local.get $addr))
                         (i32.load offset=56 (local.get $addr)))))))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $continue)))
    (local.get $acc)))
