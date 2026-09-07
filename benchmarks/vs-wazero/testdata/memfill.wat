;; memfill.wat -- memory.fill kernels, from tiny constant-size clears up to
;; multi-megabyte buffer clears. Exercises every path of the memory.fill
;; lowering: the constant-size inline stores, the runtime-length inline loop,
;; and the large zero-fill memclr fast path.
;;
;; Rebuild with: wat2wasm testdata/memfill.wat -o testdata/memfill.wasm
(module
  (memory (export "memory") 80) ;; 5 MiB, enough for a 4 MiB fill.

  ;; fill runs `iters` memory.fill of `n` bytes of byte `v` at offset 0.
  ;; `n` is a runtime value, so this is the dynamic-length path.
  (func (export "fill") (param $n i32) (param $v i32) (param $iters i32)
    (local $i i32)
    (block $done
      (loop $l
        (br_if $done (i32.ge_u (local.get $i) (local.get $iters)))
        (memory.fill (i32.const 0) (local.get $v) (local.get $n))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $l))))

  ;; fill_unaligned is the same, at a deliberately misaligned destination.
  (func (export "fill_unaligned") (param $n i32) (param $v i32) (param $iters i32)
    (local $i i32)
    (block $done
      (loop $l
        (br_if $done (i32.ge_u (local.get $i) (local.get $iters)))
        (memory.fill (i32.const 3) (local.get $v) (local.get $n))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $l))))

  ;; Constant-size clears, the shape a C/C++ or Rust producer emits for
  ;; struct/array zero-init. Size is a constant so the inline lowering applies.
  (func (export "fill_c16") (param $iters i32) (local $i i32)
    (block $done (loop $l
      (br_if $done (i32.ge_u (local.get $i) (local.get $iters)))
      (memory.fill (i32.const 0) (i32.const 0) (i32.const 16))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br $l))))
  (func (export "fill_c64") (param $iters i32) (local $i i32)
    (block $done (loop $l
      (br_if $done (i32.ge_u (local.get $i) (local.get $iters)))
      (memory.fill (i32.const 0) (i32.const 0) (i32.const 64))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br $l))))
  (func (export "fill_c128") (param $iters i32) (local $i i32)
    (block $done (loop $l
      (br_if $done (i32.ge_u (local.get $i) (local.get $iters)))
      (memory.fill (i32.const 0) (i32.const 0) (i32.const 128))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br $l))))
  (func (export "fill_c256") (param $iters i32) (local $i i32)
    (block $done (loop $l
      (br_if $done (i32.ge_u (local.get $i) (local.get $iters)))
      (memory.fill (i32.const 0) (i32.const 0) (i32.const 256))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br $l))))
  (func (export "fill_c1024") (param $iters i32) (local $i i32)
    (block $done (loop $l
      (br_if $done (i32.ge_u (local.get $i) (local.get $iters)))
      (memory.fill (i32.const 0) (i32.const 0) (i32.const 1024))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br $l)))))
