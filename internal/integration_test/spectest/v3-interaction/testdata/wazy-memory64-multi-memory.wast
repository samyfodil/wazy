;; memory64 x multi-memory, beyond the memory.copy cross product wasmtime's memory64/multi-memory.wast
;; covers.
;;
;; In a module holding both widths at once, every memory instruction has to take its operand and result
;; types from the memory its index names, not from the module. memory.size and memory.grow are the sharp
;; ones: the same opcode returns i32 for one memory and i64 for the next.

(module
  (memory $m32 1 4)
  (memory $m64 i64 1 4)
  (memory $n32 2)

  (data $d "\aa\bb\cc\dd")

  (func (export "load32") (param i32) (result i32) (i32.load8_u $m32 (local.get 0)))
  (func (export "load64") (param i64) (result i32) (i32.load8_u $m64 (local.get 0)))
  (func (export "loadn32") (param i32) (result i32) (i32.load8_u $n32 (local.get 0)))

  (func (export "store32") (param i32 i32) (i32.store8 $m32 (local.get 0) (local.get 1)))
  (func (export "store64") (param i64 i32) (i32.store8 $m64 (local.get 0) (local.get 1)))

  ;; A wide store and load, so the endianness of the 64-bit memory is exercised too.
  (func (export "store64-i64") (param i64 i64) (i64.store $m64 (local.get 0) (local.get 1)))
  (func (export "load64-i64") (param i64) (result i64) (i64.load $m64 (local.get 0)))

  ;; The result types differ per memory: i32 here, i64 next.
  (func (export "size32") (result i32) (memory.size $m32))
  (func (export "size64") (result i64) (memory.size $m64))
  (func (export "grow32") (param i32) (result i32) (memory.grow $m32 (local.get 0)))
  (func (export "grow64") (param i64) (result i64) (memory.grow $m64 (local.get 0)))

  (func (export "fill64") (param i64 i32 i64) (memory.fill $m64 (local.get 0) (local.get 1) (local.get 2)))
  (func (export "fill32") (param i32 i32 i32) (memory.fill $m32 (local.get 0) (local.get 1) (local.get 2)))

  ;; One passive segment initialising memories of both widths.
  (func (export "init64") (param i64) (memory.init $m64 $d (local.get 0) (i32.const 0) (i32.const 4)))
  (func (export "init32") (param i32) (memory.init $m32 $d (local.get 0) (i32.const 0) (i32.const 4)))
)

;; Each memory starts zeroed and is distinct from the others.
(assert_return (invoke "load32" (i32.const 0)) (i32.const 0))
(assert_return (invoke "load64" (i64.const 0)) (i32.const 0))
(assert_return (invoke "loadn32" (i32.const 0)) (i32.const 0))

;; A store into one is invisible in the others, at the same address.
(assert_return (invoke "store32" (i32.const 16) (i32.const 0x41)))
(assert_return (invoke "load32" (i32.const 16)) (i32.const 0x41))
(assert_return (invoke "load64" (i64.const 16)) (i32.const 0))
(assert_return (invoke "loadn32" (i32.const 16)) (i32.const 0))

(assert_return (invoke "store64" (i64.const 16) (i32.const 0x42)))
(assert_return (invoke "load64" (i64.const 16)) (i32.const 0x42))
(assert_return (invoke "load32" (i32.const 16)) (i32.const 0x41))

;; An address above 2^32 is reachable only on the 64-bit memory, and only once it has grown there.
(assert_trap (invoke "load64" (i64.const 0x1_0000_0000)) "out of bounds memory access")

;; A wide access on the 64-bit memory round-trips.
(assert_return (invoke "store64-i64" (i64.const 32) (i64.const 0x0123456789abcdef)))
(assert_return (invoke "load64-i64" (i64.const 32)) (i64.const 0x0123456789abcdef))
(assert_return (invoke "load64" (i64.const 32)) (i32.const 0xef))
(assert_return (invoke "load64" (i64.const 39)) (i32.const 0x01))

;; memory.size and memory.grow report in the index type of the memory they name.
(assert_return (invoke "size32") (i32.const 1))
(assert_return (invoke "size64") (i64.const 1))
(assert_return (invoke "grow32" (i32.const 1)) (i32.const 1))
(assert_return (invoke "size32") (i32.const 2))
(assert_return (invoke "size64") (i64.const 1))
(assert_return (invoke "grow64" (i64.const 2)) (i64.const 1))
(assert_return (invoke "size64") (i64.const 3))
(assert_return (invoke "size32") (i32.const 2))
;; Past the declared maximum, each reports failure in its own width.
(assert_return (invoke "grow32" (i32.const 99)) (i32.const -1))
(assert_return (invoke "grow64" (i64.const 99)) (i64.const -1))

;; Growth on the 64-bit memory made the high addresses reachable, and only there.
(assert_return (invoke "store64" (i64.const 0x1_0000) (i32.const 0x43)))
(assert_return (invoke "load64" (i64.const 0x1_0000)) (i32.const 0x43))

;; fill and init reach the memory they name, in that memory's address type.
(assert_return (invoke "fill64" (i64.const 64) (i32.const 0x7f) (i64.const 4)))
(assert_return (invoke "load64" (i64.const 65)) (i32.const 0x7f))
(assert_return (invoke "load32" (i32.const 65)) (i32.const 0))

(assert_return (invoke "fill32" (i32.const 64) (i32.const 0x7e) (i32.const 4)))
(assert_return (invoke "load32" (i32.const 65)) (i32.const 0x7e))
(assert_return (invoke "load64" (i64.const 65)) (i32.const 0x7f))

(assert_return (invoke "init64" (i64.const 128)))
(assert_return (invoke "load64" (i64.const 128)) (i32.const 0xaa))
(assert_return (invoke "load64" (i64.const 131)) (i32.const 0xdd))
(assert_return (invoke "load32" (i32.const 128)) (i32.const 0))

(assert_return (invoke "init32" (i32.const 128)))
(assert_return (invoke "load32" (i32.const 128)) (i32.const 0xaa))
(assert_return (invoke "load32" (i32.const 131)) (i32.const 0xdd))

;; Out of range on one memory does not depend on the other's size.
(assert_trap (invoke "fill32" (i32.const 0xffff_fff0) (i32.const 0) (i32.const 32)) "out of bounds memory access")
(assert_trap (invoke "init64" (i64.const 0xffff_ffff_ffff_fff0)) "out of bounds memory access")
