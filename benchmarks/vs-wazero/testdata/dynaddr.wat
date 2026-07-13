;; dynaddr.wat — a hot loop of dynamic-address i32 loads. Each base is derived
;; from the loop counter (i*k masked into [0, 0x1ff8]), so it is NOT a
;; compile-time constant: #2514/C21 cannot elide the bounds check, and every
;; distinct base is its own bounds-check site. This is the workload that
;; exercises #2515/C22 (shared trap islands): 8 check sites per iteration,
;; all trapping with the same exit code, share one per-function island.
;; Addresses stay within the 1-page (65536-byte) minimum, so no load traps.
;;
;; Regenerate: wat2wasm testdata/dynaddr.wat -o testdata/dynaddr.wasm
(module
  (memory 1)
  (func $ld (param $i i32) (param $k i32) (result i32)
    (i32.load (i32.and (i32.mul (local.get $i) (local.get $k)) (i32.const 0x1ff8))))
  (func (export "work") (param $n i32) (result i32)
    (local $acc i32) (local $i i32)
    (block $break
      (loop $cont
        (br_if $break (i32.ge_u (local.get $i) (local.get $n)))
        (local.set $acc
          (i32.add (local.get $acc)
            (i32.add
              (i32.add
                (i32.add (call $ld (local.get $i) (i32.const 1)) (call $ld (local.get $i) (i32.const 2)))
                (i32.add (call $ld (local.get $i) (i32.const 3)) (call $ld (local.get $i) (i32.const 4))))
              (i32.add
                (i32.add (call $ld (local.get $i) (i32.const 5)) (call $ld (local.get $i) (i32.const 6)))
                (i32.add (call $ld (local.get $i) (i32.const 7)) (call $ld (local.get $i) (i32.const 8)))))))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $cont)))
    (local.get $acc)))
