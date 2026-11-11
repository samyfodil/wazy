;; Selects the compiler may lower as a branch around one side's work: see select_sink_test.go.
(module
  ;; For each bit of each word, the hash folds the bit's index in when the bit is set, like
  ;; search-aho-corasick's output loop.
  (func (export "hashbits") (param $n i32) (param $seed i32) (result i32)
    (local $h i32) (local $w i32) (local $i i32)
    (local.set $h (i32.const -2128831035))
    (loop $l
      (local.set $w (i32.mul (i32.xor (local.get $i) (local.get $seed)) (i32.const -2048144789)))
      (local.set $h (select
        (i32.mul (i32.xor (i32.mul (i32.xor (local.get $h) (local.get $i)) (i32.const 16777619)) (i32.const 1)) (i32.const 16777619))
        (local.get $h)
        (i32.and (local.get $w) (i32.const 1))))
      (local.set $h (select
        (i32.mul (i32.xor (i32.mul (i32.xor (local.get $h) (local.get $i)) (i32.const 16777619)) (i32.const 2)) (i32.const 16777619))
        (local.get $h)
        (i32.and (local.get $w) (i32.const 2))))
      (local.set $h (select
        (local.get $h)
        (i32.mul (i32.xor (i32.mul (i32.xor (local.get $h) (local.get $i)) (i32.const 16777619)) (i32.const 3)) (i32.const 16777619))
        (i32.and (local.get $w) (i32.const 4))))
      (drop (local.tee $h (select
        (i32.mul (i32.xor (i32.mul (i32.xor (local.get $h) (local.get $i)) (i32.const 16777619)) (i32.const 4)) (i32.const 16777619))
        (local.get $h)
        (i32.lt_u (local.get $w) (i32.const 0x40000000)))))
      (br_if $l (i32.ne (local.tee $i (i32.add (local.get $i) (i32.const 1))) (local.get $n))))
    (local.get $h))

  (func (export "hashbits64") (param $n i32) (result i64)
    (local $h i64) (local $i i32)
    (local.set $h (i64.const 0xcbf29ce484222325))
    (loop $l
      (local.set $h (select
        (i64.mul (i64.xor (i64.rotl (local.get $h) (i64.const 5)) (i64.extend_i32_u (local.get $i))) (i64.const 0x100000001b3))
        (local.get $h)
        (i32.and (i32.mul (local.get $i) (i32.const -1640531527)) (i32.const 0x80000000))))
      (br_if $l (i32.ne (local.tee $i (i32.add (local.get $i) (i32.const 1))) (local.get $n))))
    (local.get $h))

  ;; The work's first product is also kept in a local, so it is needed either way.
  (func (export "shared") (param $n i32) (result i32)
    (local $h i32) (local $p i32) (local $i i32) (local $acc i32)
    (loop $l
      (local.set $h (select
        (i32.mul (i32.add (local.tee $p (i32.mul (local.get $h) (i32.const 31))) (local.get $i)) (i32.const 7))
        (local.get $h)
        (i32.and (local.get $i) (i32.const 3))))
      (local.set $acc (i32.add (local.get $acc) (local.get $p)))
      (br_if $l (i32.ne (local.tee $i (i32.add (local.get $i) (i32.const 1))) (local.get $n))))
    (i32.xor (local.get $h) (local.get $acc)))

  ;; The select's result goes to another local.
  (func (export "other") (param $n i32) (result i32)
    (local $h i32) (local $g i32) (local $i i32)
    (loop $l
      (local.set $g (select
        (i32.mul (i32.mul (i32.add (local.get $h) (local.get $i)) (i32.const 31)) (i32.const 7))
        (local.get $h)
        (i32.and (local.get $i) (i32.const 1))))
      (local.set $h (i32.add (local.get $g) (local.get $i)))
      (br_if $l (i32.ne (local.tee $i (i32.add (local.get $i) (i32.const 1))) (local.get $n))))
    (local.get $h))
)
