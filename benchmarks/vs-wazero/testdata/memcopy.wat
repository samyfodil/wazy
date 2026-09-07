;; memcopy.wat -- memory.copy kernels. memory.copy calls the Go runtime's
;; memmove through the same execution-context hook memory.fill uses for memclr,
;; so this is the benchmark that sees any change to that shared call sequence.
;;
;; Rebuild with: wat2wasm testdata/memcopy.wat -o testdata/memcopy.wasm
(module
  (memory (export "memory") 160) ;; 10 MiB: two non-overlapping 5 MiB halves.

  ;; copy runs `iters` memory.copy of `n` bytes from the upper half to the lower.
  (func (export "copy") (param $n i32) (param $iters i32)
    (local $i i32)
    (block $done
      (loop $l
        (br_if $done (i32.ge_u (local.get $i) (local.get $iters)))
        (memory.copy (i32.const 0) (i32.const 5242880) (local.get $n))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $l))))

  ;; Constant sizes, the shape a producer emits for a struct or array copy.
  (func (export "copy_c64") (param $iters i32) (local $i i32)
    (block $done (loop $l
      (br_if $done (i32.ge_u (local.get $i) (local.get $iters)))
      (memory.copy (i32.const 0) (i32.const 5242880) (i32.const 64))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br $l))))
  (func (export "copy_c4096") (param $iters i32) (local $i i32)
    (block $done (loop $l
      (br_if $done (i32.ge_u (local.get $i) (local.get $iters)))
      (memory.copy (i32.const 0) (i32.const 5242880) (i32.const 4096))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br $l)))))
