;; GC x multi-memory.
;;
;; array.new_data and array.init_data name a *data segment*, and array.new_elem and array.init_elem an
;; *element segment*. Neither takes a memory index, so declaring several memories must not shift what
;; those immediates mean: the segment index is a segment index however many memories the module has.
;; Active segments in the same module target memories by index, which is where the two can be confused.

(module
  (type $ret (func (result i32)))
  (type $bytes (array (mut i8)))
  (type $funcs (array (mut (ref null $ret))))

  (memory $m0 1)
  (memory $m1 1)
  (memory $m2 1)

  ;; Active into the third memory, so a module that resolved a data index as a memory index would write
  ;; into the wrong one -- and the reads below would see it.
  (data (memory $m2) (i32.const 0) "in-m2")
  ;; Segment 1 and segment 2, passive. Their indices deliberately collide with memory indices 1 and 2.
  (data $one "\11\12\13\14")
  (data $two "\21\22\23\24")

  (table $t 4 funcref)
  (elem $e (ref null $ret) (ref.func $a) (ref.func $b))
  (func $a (result i32) (i32.const 100))
  (func $b (result i32) (i32.const 200))

  (func (export "m0-byte") (param i32) (result i32) (i32.load8_u $m0 (local.get 0)))
  (func (export "m1-byte") (param i32) (result i32) (i32.load8_u $m1 (local.get 0)))
  (func (export "m2-byte") (param i32) (result i32) (i32.load8_u $m2 (local.get 0)))

  (func (export "from-one") (param i32) (result i32)
    (array.get_u $bytes (array.new_data $bytes $one (i32.const 0) (i32.const 4)) (local.get 0)))
  (func (export "from-two") (param i32) (result i32)
    (array.get_u $bytes (array.new_data $bytes $two (i32.const 0) (i32.const 4)) (local.get 0)))

  (func (export "init-from-two") (param i32) (result i32)
    (local $arr (ref $bytes))
    (local.set $arr (array.new_default $bytes (i32.const 4)))
    (array.init_data $bytes $two (local.get $arr) (i32.const 0) (i32.const 0) (i32.const 4))
    (array.get_u $bytes (local.get $arr) (local.get 0)))

  (func (export "from-elem") (param i32) (result i32)
    (call_ref $ret
      (array.get $funcs (array.new_elem $funcs $e (i32.const 0) (i32.const 2)) (local.get 0))))

  ;; memory.init on a non-zero memory from a segment whose index differs from it.
  (func (export "copy-two-into-m1")
    (memory.init $m1 $two (i32.const 8) (i32.const 0) (i32.const 4)))
)

;; The active segment landed in memory 2 and nowhere else.
(assert_return (invoke "m2-byte" (i32.const 0)) (i32.const 105)) ;; 'i'
(assert_return (invoke "m0-byte" (i32.const 0)) (i32.const 0))
(assert_return (invoke "m1-byte" (i32.const 0)) (i32.const 0))

;; Each passive segment reads as itself, not as the memory that shares its index.
(assert_return (invoke "from-one" (i32.const 0)) (i32.const 0x11))
(assert_return (invoke "from-one" (i32.const 3)) (i32.const 0x14))
(assert_return (invoke "from-two" (i32.const 0)) (i32.const 0x21))
(assert_return (invoke "from-two" (i32.const 3)) (i32.const 0x24))
(assert_return (invoke "init-from-two" (i32.const 1)) (i32.const 0x22))

;; Element segments are indexed independently of memories too.
(assert_return (invoke "from-elem" (i32.const 0)) (i32.const 100))
(assert_return (invoke "from-elem" (i32.const 1)) (i32.const 200))

;; memory.init still reaches the memory it names, with the segment it names.
(assert_return (invoke "copy-two-into-m1"))
(assert_return (invoke "m1-byte" (i32.const 8)) (i32.const 0x21))
(assert_return (invoke "m0-byte" (i32.const 8)) (i32.const 0))
