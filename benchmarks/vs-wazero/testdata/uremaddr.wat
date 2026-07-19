;; uremaddr.wat — a hot loop of dynamic-address i32 loads whose addresses are
;; bounded by unsigned remainder. The helper's base is `(i * k) % 8191`, hence
;; always in [0, 8190]. A 4-byte load therefore stays within the 1-page memory
;; minimum without a runtime bounds check.
;;
;; Regenerate: wat2wasm testdata/uremaddr.wat -o testdata/uremaddr.wasm
(module
  (memory 1)
  (func $ld (param $i i32) (param $k i32) (result i32)
    (i32.load (i32.rem_u
      (i32.mul (local.get $i) (local.get $k))
      (i32.const 8191))))
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
