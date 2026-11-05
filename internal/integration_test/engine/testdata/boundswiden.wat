;; Loads of one address at growing offsets: see boundswiden_test.go.
(module
  (memory 1)
  ;; Two fields: one check covers both.
  (func (export "pair") (param $p i32) (result i32)
    (i32.add (i32.load (local.get $p)) (i32.load offset=4 (local.get $p))))
  ;; Four fields, another address read between them.
  (func (export "four") (param $p i32) (param $q i32) (result i32)
    (i32.add
      (i32.add (i32.load8_u (local.get $p)) (i32.load (local.get $q)))
      (i32.add (i32.load16_u offset=2 (local.get $p)) (i32.add (i32.load offset=8 (local.get $p)) (i32.load offset=4 (local.get $p))))))
  ;; A store between: the first field's check cannot cover the second's, which would trap before it.
  (func (export "store") (param $p i32) (param $q i32) (result i32)
    (local $a i32)
    (local.set $a (i32.load (local.get $p)))
    (i32.store (local.get $q) (i32.const 0x55555555))
    (i32.add (local.get $a) (i32.load offset=4 (local.get $p))))
  ;; A division between: dividing by zero traps first.
  (func (export "div") (param $p i32) (param $d i32) (result i32)
    (local $a i32)
    (local.set $a (i32.div_u (i32.load (local.get $p)) (local.get $d)))
    (i32.add (local.get $a) (i32.load offset=4 (local.get $p))))
)
