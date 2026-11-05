;; Loops of the shapes the compiler turns into vector map loops (see the native frontend's
;; vecmap_plan.go), run against the interpreter by TestVecMap. Each takes byte addresses and an element
;; count, so the test chooses aliasing and bounds.
(module
  (memory (export "memory") 2)

  ;; dst[i] = a[i] * s + b[i], f64, counting i up to n by lt_s.
  (func (export "axpy") (param $dst i32) (param $a i32) (param $b i32) (param $n i32) (param $s f64)
    (local $i i32)
    (loop $l
      (f64.store
        (i32.add (local.get $dst) (i32.shl (local.get $i) (i32.const 3)))
        (f64.add
          (f64.mul (f64.load (i32.add (local.get $a) (i32.shl (local.get $i) (i32.const 3)))) (local.get $s))
          (f64.load (i32.add (local.get $b) (i32.shl (local.get $i) (i32.const 3))))))
      (br_if $l (i32.lt_s (local.tee $i (i32.add (local.get $i) (i32.const 1))) (local.get $n)))))

  ;; p[i] = sqrt(abs(p[i])) / 2 - (-p[i]), f32 in place, the counter counting down to zero by ne.
  (func (export "inplace32") (param $p i32) (param $n i32)
    (loop $l
      (f32.store (local.get $p)
        (f32.sub
          (f32.div (f32.sqrt (f32.abs (f32.load (local.get $p)))) (f32.const 2))
          (f32.neg (f32.load (local.get $p)))))
      (local.set $p (i32.add (local.get $p) (i32.const 4)))
      (br_if $l (local.tee $n (i32.sub (local.get $n) (i32.const 1))))))

  ;; Unrolled by two, as clang emits: d[2i] = x[2i] * c[0], d[2i+1] = x[2i+1] * c[0], with c[0] loaded
  ;; every iteration and the counter going up to a limit by lt_u. n counts pairs.
  (func (export "unrolled") (param $d i32) (param $x i32) (param $c i32) (param $n i32)
    (local $i i32)
    (loop $l
      (f64.store offset=0 (i32.add (local.get $d) (local.get $i))
        (f64.mul (f64.load offset=0 (i32.add (local.get $x) (local.get $i))) (f64.load (local.get $c))))
      (f64.store offset=8 (i32.add (local.get $d) (local.get $i))
        (f64.mul (f64.load offset=8 (i32.add (local.get $x) (local.get $i))) (f64.load (local.get $c))))
      (br_if $l (i32.lt_u (local.tee $i (i32.add (local.get $i) (i32.const 16)))
                          (i32.shl (local.get $n) (i32.const 4))))))

  ;; A sum of i64 elements, testing its exit at the top as a while loop does.
  (func (export "isum") (param $p i32) (param $n i32) (result i64)
    (local $i i32) (local $s i64)
    (block $out
      (loop $l
        (br_if $out (i32.ge_s (local.get $i) (local.get $n)))
        (local.set $s (i64.add (local.get $s)
          (i64.load (i32.add (local.get $p) (i32.shl (local.get $i) (i32.const 3))))))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $l)))
    (local.get $s))

  ;; An i32 xor of elements, counting down to zero tested at the top by eqz.
  (func (export "ixor") (param $p i32) (param $n i32) (result i32)
    (local $s i32)
    (local.set $s (i32.const 0x5a5a))
    (block $out
      (loop $l
        (br_if $out (i32.eqz (local.get $n)))
        (local.set $s (i32.xor (i32.load (local.get $p)) (local.get $s)))
        (local.set $p (i32.add (local.get $p) (i32.const 4)))
        (local.set $n (i32.sub (local.get $n) (i32.const 1)))
        (br $l)))
    (local.get $s))

  ;; An i32 and of a computed element: every bit the elements share.
  (func (export "iand") (param $p i32) (param $n i32) (result i32)
    (local $i i32) (local $s i32)
    (local.set $s (i32.const -1))
    (loop $l
      (local.set $s (i32.and (local.get $s)
        (i32.or (i32.load offset=4 (i32.add (local.get $p) (i32.shl (local.get $i) (i32.const 2)))) (i32.const 0x100))))
      (br_if $l (i32.lt_u (local.tee $i (i32.add (local.get $i) (i32.const 1))) (local.get $n))))
    (local.get $s))

  ;; d[i] = ((a[i] * 3) ^ b[i]) >>u 2 + (a[i] << 5) - (a[i] >>s 3) - k, i32.
  (func (export "imap32") (param $d i32) (param $a i32) (param $b i32) (param $n i32) (param $k i32)
    (local $i i32) (local $x i32)
    (loop $l
      (local.set $x (i32.load (i32.add (local.get $a) (i32.shl (local.get $i) (i32.const 2)))))
      (i32.store (i32.add (local.get $d) (i32.shl (local.get $i) (i32.const 2)))
        (i32.sub
          (i32.sub
            (i32.add
              (i32.shr_u (i32.xor (i32.mul (local.get $x) (i32.const 3))
                                  (i32.load (i32.add (local.get $b) (i32.shl (local.get $i) (i32.const 2)))))
                         (i32.const 2))
              (i32.shl (local.get $x) (i32.const 5)))
            (i32.shr_s (local.get $x) (i32.const 3)))
          (local.get $k)))
      (br_if $l (i32.lt_s (local.tee $i (i32.add (local.get $i) (i32.const 1))) (local.get $n)))))

  ;; d[i] = (x[i] >>u 7) + (x[i] << 3) - (y[i] & m) | 0x1000, i64, with m a local.
  (func (export "imap64") (param $d i32) (param $x i32) (param $y i32) (param $n i32) (param $m i64)
    (local $i i32)
    (loop $l
      (i64.store (i32.add (local.get $d) (local.get $i))
        (i64.or
          (i64.sub
            (i64.add (i64.shr_u (i64.load (i32.add (local.get $x) (local.get $i))) (i64.const 7))
                     (i64.shl (i64.load (i32.add (local.get $x) (local.get $i))) (i64.const 3)))
            (i64.and (i64.load (i32.add (local.get $y) (local.get $i))) (local.get $m)))
          (i64.const 0x1000)))
      (br_if $l (i32.ne (local.tee $i (i32.add (local.get $i) (i32.const 8))) (i32.shl (local.get $n) (i32.const 3))))))

  ;; p[i] = p[i] * 2, the store's address computing p + 8 where the load's offset adds 8: the same element
  ;; until p + 8 wraps, when the store reaches address zero and the load past 4 GiB.
  (func (export "wrapped") (param $p i32) (param $n i32)
    (loop $l
      (f64.store (i32.add (local.get $p) (i32.const 8))
        (f64.mul (f64.load offset=8 (local.get $p)) (f64.const 2)))
      (local.set $p (i32.add (local.get $p) (i32.const 8)))
      (br_if $l (local.tee $n (i32.sub (local.get $n) (i32.const 1))))))

  ;; d[i] = a[i] + 1, with a load of q[i] nothing uses: it still traps where q runs off the end.
  (func (export "unused") (param $d i32) (param $a i32) (param $q i32) (param $n i32)
    (local $i i32) (local $t f64)
    (loop $l
      (local.set $t (f64.load (i32.add (local.get $q) (i32.shl (local.get $i) (i32.const 3)))))
      (f64.store (i32.add (local.get $d) (i32.shl (local.get $i) (i32.const 3)))
        (f64.add (f64.load (i32.add (local.get $a) (i32.shl (local.get $i) (i32.const 3)))) (f64.const 1)))
      (br_if $l (i32.lt_s (local.tee $i (i32.add (local.get $i) (i32.const 1))) (local.get $n)))))

  ;; p[i] = x, x going down by two each iteration: the stored value is a counter, which each lane holds
  ;; as it is in that lane's iteration.
  (func (export "counter") (param $p i32) (param $x i32) (param $n i32)
    (loop $l
      (i32.store (local.get $p) (local.get $x))
      (local.set $x (i32.add (local.get $x) (i32.const -2)))
      (local.set $p (i32.add (local.get $p) (i32.const 4)))
      (br_if $l (local.tee $n (i32.add (local.get $n) (i32.const -1))))))
;; The sums a linear regression of hashed samples takes: i32 values hashed from a counter, zero-extended,
  ;; and four i64 totals, two of them of products.
  (func (export "linreg") (param $n i32) (param $seed i32) (result i64)
    (local $sx i64) (local $sy i64) (local $sxy i64) (local $sxx i64) (local $s i32) (local $t i32) (local $x i64) (local $y i64)
    (local.set $s (local.get $seed))
    (if (local.get $n) (then
      (loop $l
        (local.set $sy (i64.add (local.get $sy) (local.tee $y (i64.extend_i32_u (i32.and
          (i32.xor (i32.shr_u (local.tee $t (i32.mul (i32.xor (i32.shr_u (local.tee $t (i32.add (local.get $s) (i32.const 1))) (i32.const 16)) (local.get $t)) (i32.const -2048144789))) (i32.const 13)) (local.get $t))
          (i32.const 1023))))))
        (local.set $x (i64.extend_i32_u (i32.and
          (i32.xor (i32.shr_u (local.tee $t (i32.mul (i32.xor (i32.shr_u (local.get $s) (i32.const 16)) (local.get $s)) (i32.const -2048144789))) (i32.const 13)) (local.get $t))
          (i32.const 1023))))
        (local.set $sxy (i64.add (i64.mul (local.get $x) (local.get $y)) (local.get $sxy)))
        (local.set $sx (i64.add (local.get $sx) (local.get $x)))
        (local.set $sxx (i64.add (i64.mul (local.get $x) (local.get $x)) (local.get $sxx)))
        (local.set $s (i32.add (local.get $s) (i32.const 2)))
        (br_if $l (local.tee $n (i32.add (local.get $n) (i32.const -1)))))))
    (i64.xor (i64.xor (local.get $sx) (i64.rotl (local.get $sy) (i64.const 16)))
      (i64.xor (i64.rotl (local.get $sxy) (i64.const 32)) (i64.rotl (local.get $sxx) (i64.const 48)))))

  ;; Like linreg, but the hashes are whole i32s, so the zero-extensions and the products are not trivial:
  ;; the products reach 2^64 and the sums wrap.
  (func (export "linregwide") (param $n i32) (param $seed i32) (result i64)
    (local $sx i64) (local $sxy i64) (local $s i32) (local $t i32) (local $x i64) (local $y i64)
    (local.set $s (local.get $seed))
    (loop $l
      (local.set $y (i64.extend_i32_u (i32.mul (i32.xor (local.get $s) (i32.shr_u (local.get $s) (i32.const 7))) (i32.const 0x9e3779b1))))
      (local.set $x (i64.extend_i32_u (i32.xor (i32.shr_u (local.tee $t (i32.mul (i32.add (local.get $s) (i32.const 5)) (i32.const -2048144789))) (i32.const 13)) (local.get $t))))
      (local.set $sxy (i64.add (local.get $sxy) (i64.mul (local.get $x) (local.get $y))))
      (local.set $sx (i64.xor (local.get $sx) (i64.shl (local.get $x) (i64.const 17))))
      (local.set $s (i32.add (local.get $s) (i32.const 3)))
      (br_if $l (local.tee $n (i32.add (local.get $n) (i32.const -1)))))
    (i64.xor (local.get $sx) (i64.rotl (local.get $sxy) (i64.const 7))))

  ;; An i32 sum of a counter going up by three: four i32 lanes to a vector.
  (func (export "iotasum32") (param $i i32) (param $end i32) (result i32)
    (local $acc i32)
    (loop $l
      (local.set $acc (i32.add (local.get $acc)
        (i32.xor (i32.mul (local.get $i) (i32.const 0x9e3779b1)) (i32.shr_u (local.get $i) (i32.const 3)))))
      (br_if $l (i32.lt_s (local.tee $i (i32.add (local.get $i) (i32.const 3))) (local.get $end))))
    (local.get $acc))

  ;; A counter going down by five, an i32 xor total and an i64 sum of the same values.
  (func (export "mixred") (param $i i32) (param $n i32) (result i64)
    (local $x i32) (local $wide i64) (local $t i32)
    (loop $l
      (local.set $t (i32.sub (i32.mul (local.get $i) (local.get $i)) (local.get $n)))
      (local.set $x (i32.xor (local.get $x) (local.get $t)))
      (local.set $wide (i64.add (local.get $wide) (i64.extend_i32_u (local.get $t))))
      (local.set $i (i32.sub (local.get $i) (i32.const 5)))
      (br_if $l (local.tee $n (i32.add (local.get $n) (i32.const -1)))))
    (i64.xor (local.get $wide) (i64.shl (i64.extend_i32_u (local.get $x)) (i64.const 40))))

  ;; And and or totals of hashed counter values, one of them i64.
  (func (export "andor") (param $i i32) (param $n i32) (result i64)
    (local $a i32) (local $o i64)
    (local.set $a (i32.const -1))
    (loop $l
      (local.set $a (i32.and (local.get $a) (i32.or (i32.mul (local.get $i) (i32.const 0x01000193)) (i32.const 0x10))))
      (local.set $o (i64.or (local.get $o) (i64.shl (i64.extend_i32_u (i32.and (local.get $i) (i32.const 0xff))) (i64.const 24))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (local.tee $n (i32.add (local.get $n) (i32.const -1)))))
    (i64.xor (local.get $o) (i64.extend_i32_u (local.get $a))))
)
